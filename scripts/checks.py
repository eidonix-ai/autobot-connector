#!/usr/bin/env python3
import base64
import contextlib
from datetime import datetime
import io
import ipaddress
import json
import os
from pathlib import Path
import select
import shlex
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import time


def serve(protocol, host):
    family = socket.AF_INET6 if protocol == "tcp6" else socket.AF_INET
    kind = socket.SOCK_DGRAM if protocol == "dns" else socket.SOCK_STREAM
    with socket.socket(family, kind) as server:
        server.bind((host, 0))
        if protocol != "dns":
            server.listen()
        print(server.getsockname()[1], flush=True)
        while True:
            if protocol == "dns":
                query, peer = server.recvfrom(512)
                server.sendto(query[:2] + bytes([0x81, 0x83]) + query[4:], peer)
            else:
                conn, _ = server.accept()
                with conn:
                    conn.sendall(b"stage1")


if len(sys.argv) > 1 and sys.argv[1] == "listen":
    serve(*sys.argv[2:])
    sys.exit()

REBOOT = Path("reboot-state.log")
MODE = sys.argv[-1] if sys.argv[-1] in {"reboot-before", "reboot-after"} else "checks"
if MODE == "reboot-after":
    reboot = json.loads(REBOOT.read_text())
    BIN, SOCKET, RUNNER, WORK = reboot["args"]
else:
    BIN, SOCKET, RUNNER, WORK = sys.argv[1:5]
FIXTURE = [BIN, "fixture", SOCKET]
LABEL = "org.flare.donor.lease"
MARKERS = "for d in /work /tmp /dev/shm; do printf stage1 > \"$d/marker\"; done; echo marker-streamed"
children = []


def announce(args):
    print("+", shlex.join(map(str, args)), flush=True)


def run(*args, expected=0, quiet=False, timeout=15):
    if not quiet:
        announce(args)
    p = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    out = p.stdout + p.stderr
    if not quiet and out:
        print(out, end="", flush=True)
    if expected is not None and p.returncode != expected:
        raise RuntimeError(f"{args[0]} exited {p.returncode}; expected {expected}: {out}")
    return p


def spawn(*args):
    announce(args)
    p = subprocess.Popen(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    children.append(p)
    return p


def stop_child(p, sig=signal.SIGTERM):
    if p.poll() is None:
        print(f"sending {sig.name} to recorded PID {p.pid}")
        p.send_signal(sig)
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait(timeout=5)


def live_line(p):
    # These children always emit their readiness line, including when they fail.
    assert select.select([p.stdout], [], [], 12)[0], f"no output from PID {p.pid}"
    line = p.stdout.readline().strip()
    print(line, flush=True)
    return line


def cli(*args, **kwargs):
    return run(*FIXTURE, *args, **kwargs)


def status():
    p = cli("status", expected=None, quiet=True, timeout=6)
    return json.loads(p.stdout) if p.returncode == 0 else {}


def wait_for(get, seconds, name):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        value = get()
        if value:
            return value
        time.sleep(.15)
    raise AssertionError(f"timed out: {name}")


def ready(lease=None, end=None, seconds=30, since=None):
    previous = None
    def check():
        nonlocal previous
        data = status()
        if data.get("status") == "ready":
            return data
        condition = data.get("status"), data.get("reason")
        if condition != previous:
            print("waiting for ready:", condition)
            previous = condition
        return None
    data = wait_for(check, seconds, "runner ready")
    elapsed = time.monotonic() - since if since is not None else 0
    if since is not None:
        assert elapsed <= seconds, elapsed
    logged = json.loads(json.dumps(data))
    if "salvage_tar" in logged.get("last", {}):
        archive = logged["last"].pop("salvage_tar")
        logged["last"]["salvage_bytes"] = len(base64.b64decode(archive))
    print("ready:", json.dumps(logged), f"elapsed={elapsed:.3f}s", flush=True)
    if lease:
        assert data["last"]["lease"] == lease and data["last"]["end"] == end, data
    return data


def inspect(container):
    return json.loads(run("docker", "inspect", container, quiet=True).stdout)[0]


def start(name, seconds=40, *salvage):
    j = json.loads(cli("run", name, f"{seconds}s", *salvage).stdout)
    with open("containers.log", "a") as f:
        f.write(j["container"] + "\n")
    return j


def job(j, *argv, **kwargs):
    return cli("exec", j["lease"], *argv, **kwargs)


def group(container):
    pid = inspect(container)["State"]["Pid"]
    line = Path(f"/proc/{pid}/cgroup").read_text().strip()
    assert line.startswith("0::/"), line
    return Path("/sys/fs/cgroup" + line[3:])


def snapshot(container):
    cg = group(container)
    pids = {int(n) for n in (cg / "cgroup.procs").read_text().split()}
    assert pids
    print(f"host job cgroup={cg} pids={sorted(pids)}")
    return cg, {pid: identity(pid) for pid in pids}


def identity(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]
    except FileNotFoundError:
        return None


def absent(j, old):
    cg, pids = old
    assert not cg.exists(), cg
    assert all(identity(pid) != born for pid, born in pids.items()), pids
    p = run("docker", "ps", "-aq", "--filter", f"label={LABEL}={j['lease']}")
    assert not p.stdout.strip(), p.stdout
    print("teardown: label absent, cgroup absent, recorded processes gone")


def no_markers(j):
    next_job = start(j["lease"] + "-next")
    job(next_job, "/bin/sh", "-c", 'for d in /work /tmp /dev/shm; do test ! -e "$d/marker" || exit 1; done; echo "second lease has no markers"')
    old = snapshot(next_job["container"])
    cli("finish", next_job["lease"])
    ready(next_job["lease"], "complete")
    absent(next_job, old)


def finish(j, old, operation="finish", end="complete"):
    since = time.monotonic()
    cli(operation, j["lease"])
    record = ready(j["lease"], end, since=since)["last"]
    absent(j, old)
    return record


def counters(path):
    return {key: int(value) for key, value in (line.split() for line in path.read_text().splitlines())}


def profile():
    j = start("profile")
    container = j["container"]
    data = json.loads(run("docker", "inspect", container).stdout)[0]
    h = data["HostConfig"]
    assert data["Config"]["User"] == "1000:1000" and data["Config"]["Image"] == WORK
    assert h["ReadonlyRootfs"] and h["NetworkMode"] == "none" and not h["Privileged"]
    assert h["CapDrop"] == ["ALL"] and h["SecurityOpt"] == ["no-new-privileges"]
    assert h["PidMode"] != "host" and h["IpcMode"] != "host"
    devices = h["DeviceRequests"]
    assert len(devices) == 1 and devices[0]["Driver"] == "cdi" and devices[0]["DeviceIDs"] == ["flare.local/gpu=0"]
    assert h["RestartPolicy"]["Name"] in ("", "no") and not h["Binds"] and not data["Config"]["Volumes"]
    assert h["Memory"] == h["MemorySwap"] == 536870912
    assert h["CpuQuota"] == h["CpuPeriod"] == 100000 and h["PidsLimit"] == 48
    announce([*FIXTURE, "exec", j["lease"], "/stage1", "profile"])
    p = json.loads(job(j, "/stage1", "profile", quiet=True).stdout)
    print(p["Mounts"], end="")
    print("devices:", p["Devices"])
    print("AppArmor:", p["AppArmor"])
    for field in ["Uid", "Gid", "Groups", "CapEff", "CapBnd", "NoNewPrivs", "Seccomp"]:
        print(field, p["Status"][field])
    assert p["Status"]["Uid"] == "1000 1000 1000 1000" and p["Status"]["CapEff"] == "0" * 16
    assert p["Status"]["Seccomp"] == "2" and p["AppArmor"] == "docker-default (enforce)"
    cid = data["Id"]
    cg = group(cid)
    for k, expected in {"cpu.max": "100000 100000", "memory.max": "536870912", "memory.swap.max": "0", "pids.max": "48"}.items():
        actual = (cg / k).read_text().strip()
        print(k, actual)
        assert actual == expected
    sock = Path(SOCKET)
    assert stat.S_IMODE(sock.stat().st_mode) == 0o700 and sock.stat().st_uid == 1000
    r = inspect(RUNNER)
    assert r["HostConfig"]["NetworkMode"] == "none" and r["HostConfig"]["PidMode"] == "host"
    finish(j, snapshot(cid))
    print("PASS 1 profile and named Docker system mount exceptions")


def network():
    with contextlib.ExitStack() as clean:
        def listener(protocol, host, prefix=()):
            p = spawn(*prefix, sys.executable, "-u", __file__, "listen", protocol, host)
            clean.callback(stop_child, p)
            port = int(live_line(p))
            return f"[{host}]:{port}" if protocol == "tcp6" else f"{host}:{port}"
        loop4 = socket.gethostbyname("localhost")
        loop6 = socket.inet_ntop(socket.AF_INET6, (1).to_bytes(16, "big"))
        targets = [("host", "tcp4", listener("tcp4", loop4)), ("IPv6", "tcp6", listener("tcp6", loop6)),
                   ("DNS", "dns", listener("dns", loop4))]
        suffix = f"{os.getpid() & 0xffff:x}"
        ns, left, right = ("flare-" + suffix, "flh" + suffix, "flp" + suffix)
        subnet = ipaddress.IPv4Network((ipaddress.IPv4Address(bytes([198, 18, (os.getpid() >> 6) % 256, (os.getpid() * 4) % 256])), 30))
        a, b = list(subnet.hosts())
        run("ip", "netns", "add", ns)
        clean.callback(run, "ip", "netns", "del", ns)
        run("ip", "link", "add", left, "type", "veth", "peer", "name", right)
        clean.callback(run, "ip", "link", "del", left)
        run("ip", "link", "set", right, "netns", ns)
        run("ip", "addr", "add", f"{a}/30", "dev", left)
        run("ip", "link", "set", left, "up")
        run("ip", "-n", ns, "addr", "add", f"{b}/30", "dev", right)
        run("ip", "-n", ns, "link", "set", right, "up")
        targets.append(("LAN peer", "tcp4", listener("tcp4", str(b), ("ip", "netns", "exec", ns))))
        port = 19000 + os.getpid() % 10000
        other = run("docker", "create", "--network", "bridge", WORK, "/stage1", "probe", "listen", f":{port}").stdout.strip()
        with open("containers.log", "a") as f:
            f.write(other + "\n")
        clean.callback(run, "docker", "rm", "-f", other)
        run("docker", "start", other)
        targets.append(("sibling container", "tcp4", f"{inspect(other)['NetworkSettings']['Networks']['bridge']['IPAddress']}:{port}"))
        j = start("network")
        for name, proto, address in targets:
            wait_for(lambda: run(BIN, "probe", "connect", proto, address, expected=None, quiet=True).returncode == 0, 5, name)
            run(BIN, "probe", "connect", proto, address)
            p = job(j, "/stage1", "probe", "connect", proto, address, expected=1)
            assert "refused" in p.stderr or "unreachable" in p.stderr or "i/o timeout" in p.stderr, p.stderr
            print(f"{name}: host control succeeded, job denied")
        job(j, "/bin/sh", "-c", 'test ! -e /control && test ! -e /var/run/docker.sock && echo "control and Docker sockets absent"')
        finish(j, snapshot(j["container"]))
    print("PASS 2 network")


def oom(j):
    cg = group(j["container"])
    before = counters(cg / "memory.events")["oom_kill"]
    pid = inspect(j["container"])["State"]["Pid"]
    job(j, "/stage1", "probe", "memory", "768", expected=137)
    after = counters(cg / "memory.events")["oom_kill"]
    print(f"memory.events oom_kill {before} -> {after}; command 137; keeper PID {pid}")
    assert after > before and inspect(j["container"])["State"]["Pid"] == pid
    assert inspect(RUNNER)["State"]["Running"] and status()["status"] == "running"


def limits():
    j = start("limits")
    for mount, size in [("/work", 16 << 20), ("/tmp", 8 << 20), ("/dev/shm", 8 << 20)]:
        p = job(j, "/stage1", "probe", "fill", mount, "bytes")
        assert p.stdout.strip().endswith(f"ENOSPC after {size}"), p.stdout
    p = job(j, "/stage1", "probe", "fill", "/work", "inodes")
    assert p.stdout.strip().endswith("ENOSPC after 127"), p.stdout
    cg = group(j["container"])
    before = counters(cg / "pids.events")["max"]
    p = job(j, "/stage1", "probe", "fork")
    after = counters(cg / "pids.events")["max"]
    peak = (cg / "pids.peak").read_text().strip()
    print(f"pids.peak={peak}; pids.events max {before} -> {after}")
    assert "EAGAIN" in p.stdout and after > before and peak == "48"
    before = counters(cg / "cpu.stat")
    started = time.monotonic()
    job(j, "/stage1", "probe", "cpu")
    elapsed = time.monotonic() - started
    after = counters(cg / "cpu.stat")
    used = after["usage_usec"] - before["usage_usec"]
    throttle = after["nr_throttled"] - before["nr_throttled"]
    print(f"cpu.stat usage_usec_delta={used} nr_throttled_delta={throttle} elapsed={elapsed:.3f}s")
    assert 2_300_000 < used < 3_300_000 and throttle > 0
    oom(j)
    result = finish(j, snapshot(j["container"]))
    assert result["oom_after"] > result["oom_before"] and isinstance(result["oom_killed"], bool)
    print("PASS 3 limits")


def gpu_pids():
    p = run("nvidia-smi", "--query-compute-apps=pid", "--format=csv", quiet=True)
    lines = p.stdout.strip().splitlines()
    assert lines and lines[0] == "pid", p.stdout
    return {int(line) for line in lines[1:]}


def mem_available():
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemAvailable:"):
            return int(line.split()[1])
    raise AssertionError("MemAvailable missing")


def gpu():
    run("nvidia-smi", "--query-compute-apps=pid", "--format=csv")
    assert gpu_pids() == set()
    j = start("cuda", 40)
    cg = group(j["container"])
    current = cg / "memory.current"
    before = mem_available(), int(current.read_text())
    p = spawn(*FIXTURE, "exec", j["lease"], "/cuda")
    assert live_line(p).startswith("SM121 uid=1000 "), "CUDA probe failed"
    old = snapshot(j["container"])
    query = run("nvidia-smi", "--query-compute-apps=pid", "--format=csv")
    visible = gpu_pids()
    assert len(visible) == 1 and visible.issubset(old[1]), query.stdout
    inside = run("docker", "exec", RUNNER, "/usr/bin/nvidia-smi", "--query-compute-apps=pid", "--format=csv")
    assert all(str(pid) in inside.stdout.splitlines() for pid in visible), inside.stdout
    during = mem_available(), int(current.read_text())
    since = time.monotonic()
    cli("stop", j["lease"])
    record = ready(j["lease"], "cancel", since=since)["last"]
    absent(j, old)
    assert p.wait(timeout=5) == 137
    run("nvidia-smi", "--query-compute-apps=pid", "--format=csv")
    after = mem_available()
    elapsed = time.monotonic() - since
    print(f"GPU memory: before MemAvailable={before[0]} KiB cgroup={before[1]} B; during MemAvailable={during[0]} KiB cgroup={during[1]} B; after MemAvailable={after} KiB cgroup_at_quiesce={record['memory_at_quiesce']} B, removed afterward")
    print(f"GPU deltas: available={(during[0]-before[0])*1024} B cgroup={during[1]-before[1]} B; recovery={elapsed:.3f}s")
    assert record["commands"][-1]["exit_code"] == 137
    assert not gpu_pids() and abs(after - before[0]) * 1024 <= 1_000_000_000 and elapsed <= 30
    print("PASS 4 SM121 GPU and recovery")


def marker(j):
    return job(j, "/bin/sh", "-c", MARKERS)


def running_marker(j):
    p = spawn(*FIXTURE, "exec", j["lease"], "/bin/sh", "-c", MARKERS + "; exec /bin/sleep 60")
    assert live_line(p) == "marker-streamed"
    return p


def evidence(record, code):
    assert record["commands"][-1]["exit_code"] == code, record
    assert isinstance(record["oom_killed"], bool) and record["oom_before"] is not None and record["oom_after"] is not None
    with tarfile.open(fileobj=io.BytesIO(base64.b64decode(record["salvage_tar"]))) as f:
        assert f.getnames() == ["marker"]
        with f.extractfile("marker") as saved:
            assert saved.read() == b"stage1"
    print(f"salvage: marker verified; exit={code}; OOMKilled={record['oom_killed']}")


def lost(j, record):
    assert record["end"] == "lost" and record["lease"] == j["lease"]
    assert record.get("oom_killed") is None and not record.get("salvage_tar") and not record.get("commands")
    print("previously streamed output survived; salvage and final exit facts marked lost")


def teardown():
    for name, end in [("normal", "complete"), ("cancel", "cancel"), ("deadline", "deadline"),
                      ("oom", "complete"), ("fixture-killed", "deadline"),
                      ("runner-killed", "lost"), ("daemon-restart", "lost")]:
        Path("progress").write_text(f"5 teardown {name}\n")
        j = start(name, 8 if end == "deadline" else 45, "marker")
        p = None
        code = 0
        if name in ("normal", "oom"):
            marker(j)
            if name == "normal":
                job(j, "/stage1", "probe", "orphan")
            else:
                oom(j)
                code = 137
        else:
            p = running_marker(j)
            code = 137
        old = snapshot(j["container"])
        if name in ("normal", "oom"):
            result = finish(j, old)
        elif name == "cancel":
            result = finish(j, old, "stop", "cancel")
        elif end == "deadline":
            if name == "fixture-killed":
                assert p is not None
                stop_child(p, signal.SIGKILL)
            deadline = datetime.fromisoformat(j["deadline"]).timestamp()
            deadline_mono = time.monotonic() + deadline - time.time()
            result = ready(name, end, seconds=30, since=deadline_mono)["last"]
            absent(j, old)
        else:
            since = time.monotonic()
            if name == "runner-killed":
                run("kill", "-KILL", str(inspect(RUNNER)["State"]["Pid"]))
                limit = 60
            else:
                run("systemctl", "restart", "docker", timeout=110)
                wait_for(lambda: run("docker", "info", quiet=True, expected=None, timeout=6).returncode == 0, 15, "daemon restarted")
                since, limit = time.monotonic(), 120
            result = ready(name, end, seconds=limit, since=since)["last"]
            absent(j, old)
        if end == "lost":
            lost(j, result)
        else:
            evidence(result, code)
        if p is not None and name != "fixture-killed":
            rest, _ = p.communicate(timeout=8)
            if rest:
                print(rest, end="")
            if end != "lost":
                assert p.returncode == code, p.returncode
        no_markers(j)
        print(f"PASS 5 {name}")


def refusal():
    leftover = run("docker", "create", "--label", LABEL + "=refusal-leftover", WORK, "/stage1", "keeper").stdout.strip()
    with open("containers.log", "a") as f:
        f.write(leftover + "\n")
    try:
        p = cli("run", "refused-leftover", "15s", expected=1)
        assert "leftover labelled container" in p.stderr
        assert "leftover labelled container" in status()["reason"]
    finally:
        run("docker", "rm", leftover)
    ready()
    before = status()["baseline_kib"]
    assert mem_available() > (8 << 20)
    p = spawn(BIN, "probe", "memory", "3072")
    try:
        assert live_line(p) == "touched 3072 MiB"
        print(f"memory refusal: baseline={before} KiB held={mem_available()} KiB")
        denied = cli("run", "refused-memory", "15s", expected=1)
        assert "below recovery floor" in denied.stderr and "below recovery floor" in status()["reason"]
    finally:
        stop_child(p)
    ready()
    print("PASS 6 leftover container and memory refusal")


def boot_id():
    return Path("/proc/sys/kernel/random/boot_id").read_text().strip()


def prepare_reboot():
    ready(seconds=25)
    j = start("host-reboot", 600, "marker")
    with open("reboot-stream.log", "w") as f:
        args = [*FIXTURE, "exec", j["lease"], "/bin/sh", "-c", MARKERS + "; exec /bin/sleep 600"]
        announce(args)
        p = subprocess.Popen(args, stdout=f, stderr=subprocess.STDOUT, start_new_session=True)
    children.append(p)
    wait_for(lambda: "marker-streamed" in Path("reboot-stream.log").read_text(), 10, "reboot marker")
    old = snapshot(j["container"])
    with REBOOT.open("w") as f:
        json.dump({"args": [BIN, SOCKET, RUNNER, WORK], "job": j, "boot": boot_id(),
                   "cgroup": str(old[0]), "pids": old[1], "fixture_pid": p.pid}, f)
        f.flush()
        os.fsync(f.fileno())
    children.remove(p)
    print("reboot armed; the fixture streamed its marker and the job is still running")


def verify_reboot():
    assert boot_id() != reboot["boot"], "host boot identifier did not change"
    wait_for(lambda: run("docker", "info", quiet=True, expected=None).returncode == 0, 120, "Docker available")
    started = run("systemctl", "show", "docker", "--property=ActiveEnterTimestampMonotonic", "--value")
    since = int(started.stdout) / 1_000_000
    assert since > 0
    j = reboot["job"]
    result = ready(j["lease"], "lost", seconds=120, since=since)["last"]
    absent(j, (Path(reboot["cgroup"]), {int(pid): born for pid, born in reboot["pids"].items()}))
    assert "marker-streamed" in Path("reboot-stream.log").read_text()
    lost(j, result)
    no_markers(j)
    print("PASS 5 host reboot")


def interrupted(*_):
    raise SystemExit(124)


signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGINT, interrupted)
try:
    if MODE == "reboot-before":
        prepare_reboot()
    elif MODE == "reboot-after":
        verify_reboot()
    else:
        ready(seconds=25)
        for label, check in [("1 profile", profile), ("2 network", network), ("3 limits", limits),
                             ("4 GPU", gpu), ("5 teardown", teardown), ("6 refusal", refusal)]:
            Path("progress").write_text(label + "\n")
            check()
        print("DEFERRED 5 host reboot; operator must start it")
finally:
    for child in children:
        stop_child(child)
