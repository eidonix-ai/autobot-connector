#!/usr/bin/env python3
import base64
import ctypes
import io
import json
import mmap
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import tarfile
import time


def memory():
    values = {}
    with open("/proc/meminfo") as f:
        for line in f:
            key, value = line.split(":", 1)
            if key in ("MemAvailable", "MemTotal"):
                values[key] = int(value.split()[0])
    return values["MemAvailable"], values["MemTotal"]


def pressure():
    block_size = 128 << 20
    blocks = []
    started = time.monotonic()
    while True:
        available, total = memory()
        if available < total * .102:
            raise RuntimeError(f"pressure crossed guard before CUDA: {available} KiB")
        if available <= total * .11:
            print(f"pressure ready: MemAvailable={available} KiB MemTotal={total} KiB", flush=True)
            break
        if len(blocks) * block_size > 112 << 30 or time.monotonic() - started > 85:
            raise RuntimeError("pressure cap reached before target")
        block = mmap.mmap(-1, block_size, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS)
        block.madvise(mmap.MADV_HUGEPAGE)
        ctypes.memset(ctypes.addressof(ctypes.c_char.from_buffer(block)), 1, block_size)
        blocks.append(block)
    time.sleep(70)


if len(sys.argv) == 2 and sys.argv[1] == "pressure":
    pressure()
    sys.exit(0)


BUILD, HUB_PID, LINK, DONOR, *MODE = sys.argv[1:]
HUB_PID = int(HUB_PID)
HUB = [str(Path(BUILD) / "hub")]
DRIVER = [str(Path(BUILD) / "driver"), os.environ["HUB_SOCKET"], DONOR]
LABEL = "org.flare.donor.lease"
EMPTY = str(Path(BUILD) / "empty.json")
children = []


def mark(s):
    print(s, flush=True)
    Path("progress").write_text(s + "\n")


def run(args, expected=0, timeout=25, quiet=False):
    if not quiet:
        print("+", shlex.join(args), flush=True)
    p = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    if not quiet:
        print((p.stdout + p.stderr).strip()[:1800], flush=True)
    assert p.returncode == expected, (args, p.returncode, p.stdout[:1000], p.stderr)
    return p


def child(args):
    print("+", shlex.join(args), flush=True)
    p = subprocess.Popen(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    children.append(p)
    return p


def stop(p):
    if p.poll() is None:
        p.terminate()
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait(timeout=5)


def poll(get, seconds, description, interval=.15):
    stop_at = time.monotonic() + seconds
    while time.monotonic() < stop_at:
        value = get()
        if value:
            return value
        time.sleep(interval)
    raise AssertionError(f"timed out: {description}")


def donor():
    p = run(HUB + ["donors"], quiet=True)
    return next(d for d in json.loads(p.stdout) if d["id"] == DONOR)


def ready(d):
    return d["connected"] and d["status"]["ready"]


def begin(name, duration="45s"):
    return run(DRIVER + ["run", name, duration]).stdout.strip()


def command(lease, *argv):
    return DRIVER + ["exec", lease, *argv]


def lease_container(lease):
    p = run(["docker", "ps", "-aq", "--no-trunc", "--filter", f"label={LABEL}={lease}"], quiet=True)
    return p.stdout.strip()


def running(lease):
    p = child(command(lease, "/bin/sh", "-c", "echo running; exec /bin/sleep 60"))
    assert p.stdout.readline().strip() == "running"
    return p


def pause():
    mark("6 pause and reconnect")
    lease = begin("pause")
    p = child(command(lease, "/bin/sh", "-c", "echo running; /bin/sleep 5; echo finished"))
    assert p.stdout.readline().strip() == "running"
    run(["docker", "compose", "exec", "-T", "link", "/link", "pause"])
    poll(lambda: donor()["status"]["paused"], 10, "pause")
    assert "paused=true" in run(HUB + ["run", DONOR, EMPTY, "--", "/bin/sh", "-c", "exit 1"], expected=1).stderr
    trailing, _ = p.communicate(timeout=12)
    assert p.returncode == 0 and trailing.strip() == "finished"
    result = json.loads(run(DRIVER + ["finish", lease]).stdout)
    assert result["verified"] and result["facts"]["end"] == "complete"
    run(["docker", "restart", LINK])
    paused = poll(lambda: donor()["connected"] and donor()["status"]["paused"] and donor(), 20, "reconnect paused")
    assert paused["id"] == DONOR and not paused["status"]["ready"]
    assert "paused=true" in run(HUB + ["run", DONOR, EMPTY, "--", "/bin/sh", "-c", "exit 1"], expected=1).stderr
    run(["docker", "compose", "exec", "-T", "link", "/link", "resume"])
    poll(lambda: ready(donor()), 12, "resume")
    print("PASS pause: running command finished, same key reconnected paused, resumed", flush=True)


def heartbeat():
    mark("2 missing heartbeat")
    lease = begin("heartbeat")
    p = running(lease)
    assert lease_container(lease)
    start = time.monotonic()
    print(f"+ kill -STOP {HUB_PID}", flush=True)
    os.kill(HUB_PID, signal.SIGSTOP)
    try:
        def gone():
            return not lease_container(lease)
        poll(gone, 12, "job stopped when hub heartbeat disappeared", .05)
        elapsed = time.monotonic() - start
        print(f"PASS missed heartbeat: container gone in {elapsed:.3f}s", flush=True)
    finally:
        os.kill(HUB_PID, signal.SIGCONT)
    p.communicate(timeout=12)
    lost = json.loads(run(DRIVER + ["wait", lease], expected=1).stdout)
    assert not lost["verified"] and lost["facts"]["end"] == "lost"
    poll(lambda: ready(donor()), 20, "reconnect after silence")


def negative_inputs():
    mark("5 input refusal and salvage")
    result = json.loads(Path("manifest-job.log").read_text().splitlines()[-1])
    assert result["verified"] and result["facts"]["commands"][0]["exit_code"] == 0
    with tarfile.open(fileobj=io.BytesIO(base64.b64decode(result["salvage"]))) as archive:
        assert archive.getnames() == ["input-checks"]
        data = archive.extractfile("input-checks").read().decode().strip()
        assert data == json.loads(Path("manifests/flare.json").read_text())["source"]["commit"]
        print(f"PASS fetched commit, locked crate, signed output and salvage commit={data}", flush=True)
    job = json.loads(Path("manifests/flare.json").read_text())
    job.pop("source")
    job["files"][0]["sha256"] = "0" * 64
    m = Path(BUILD) / "wrong.json"
    m.write_text(json.dumps(job))
    poll(lambda: ready(donor()), 12, "ready for negative inputs")
    p = run(HUB + ["run", DONOR, str(m), "--", "/bin/sh", "-c", "echo unexpected"], expected=1)
    assert "digest mismatch" in p.stderr and "unexpected" not in p.stdout
    job["files"][0]["url"] = "https://example.org/not-an-input"
    m.write_text(json.dumps(job))
    p = run(HUB + ["run", DONOR, str(m), "--", "/bin/sh", "-c", "echo unexpected"], expected=1)
    assert "outside public host list" in p.stderr and "unexpected" not in p.stdout


def cuda():
    mark("4 real CUDA memory floor")
    assert not run(["nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader"]).stdout.strip()
    lease = begin("cuda-floor", "3m")
    container = lease_container(lease)
    assert container
    p = child([sys.executable, __file__, "pressure"])
    try:
        line = p.stdout.readline().strip()
        print(line, flush=True)
        assert line.startswith("pressure ready:")
        available, total = memory()
        assert available * 10 > total
        work = child(command(lease, "/cuda"))
        crossed = None
        minimum = available
        start = time.monotonic()
        while time.monotonic() - start < 12:
            at = time.monotonic()
            current, _ = memory()
            minimum = min(current, minimum)
            if crossed is None and current * 10 < total:
                crossed = at
            if work.poll() is not None:
                break
            time.sleep(.015)
        assert crossed is not None, f"CUDA did not cross floor; minimum {minimum}, total {total}"
        poll(lambda: not lease_container(lease), 10, "CUDA container gone", .03)
        elapsed = time.monotonic() - crossed
        output, _ = work.communicate(timeout=10)
        print(output, end="", flush=True)
        assert "touched_bytes=2147483648" in output and elapsed < 5, (output, elapsed)
        result = json.loads(run(DRIVER + ["wait", lease], quiet=True).stdout)
        facts = result["facts"]
        assert result["verified"] and facts["end"] == "health" and facts["runner_end"] == "cancel", result
        assert "host memory" in facts["health"]["reasons"]
        assert facts["health"]["available_kib"] * 10 < facts["health"]["total_kib"]
        print(f"PASS CUDA stop: floor={total//10} KiB first_sample={facts['health']['available_kib']} KiB "
              f"host_min={minimum} KiB, container gone within {elapsed:.3f}s, command={facts['commands']}", flush=True)
        poll(lambda: "host memory" in donor()["status"]["health"].get("reasons", []), 10, "memory cooling reported")
        assert "host memory" in run(HUB + ["run", DONOR, EMPTY, "--", "/bin/sh", "-c", "exit 1"], expected=1).stderr
    finally:
        stop(p)
    recovered = time.monotonic()
    assert memory()[0] * 5 > total
    mark("4 waiting for five-minute host memory recovery")
    time.sleep(8)
    assert "host memory" in donor()["status"]["health"]["reasons"]
    poll(lambda: ready(donor()), 320, "memory recovery", 1)
    elapsed = time.monotonic() - recovered
    assert 295 <= elapsed <= 320, elapsed
    print(f"PASS host memory recovery: ready {elapsed:.3f}s after release", flush=True)


def gpu_partition():
    mark("4 overlapping leases with exact GPU assignments")
    assert not run(["nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader"]).stdout.strip()
    two = Path(BUILD) / "two.json"
    two.write_text('{"gpus":2}\n')
    go = [str(Path(BUILD) / "driver"), os.environ["HUB_SOCKET"], DONOR]
    first = run(go + ["begin", str(two), "2m"]).stdout.strip()
    second = run(go + ["begin", str(two), "2m"]).stdout.strip()
    assignments = []
    for lease in [first, second]:
        container = lease_container(lease)
        assert container
        data = json.loads(run(["docker", "inspect", container], quiet=True).stdout)[0]
        devices = data["HostConfig"]["DeviceRequests"][0]["DeviceIDs"]
        assert len(devices) == 2 and all(d.startswith("flare.local/gpu=GPU-") for d in devices)
        assignments.append({d.removeprefix("flare.local/gpu=") for d in devices})
        result = run(command(lease, "/cuda", "--devices"))
        assert set(result.stdout.splitlines()) == assignments[-1]
    assert not assignments[0] & assignments[1]
    p = run(HUB + ["run", DONOR, EMPTY, "--", "/bin/sh", "-c", "exit 1"], expected=1)
    assert "donor unavailable" in p.stderr
    work = [child(command(lease, "/cuda")) for lease in (first, second)]
    for p in work:
        line = p.stdout.readline().strip()
        print(line, flush=True)
        assert line.startswith("SM120 uid=1000 ")
    rows = run(["nvidia-smi", "--query-compute-apps=gpu_uuid,pid", "--format=csv,noheader"])
    active = {r.split(",")[0] for r in rows.stdout.splitlines()}
    assert len(active) == 2 and all(len(active & ids) == 1 for ids in assignments)
    for index, lease in enumerate([first, second]):
        result = json.loads(run(go + ["stop", lease]).stdout)
        assert result["verified"] and result["facts"]["end"] == "cancel"
        assert set(result["facts"]["devices"]) == assignments[index]
    poll(lambda: ready(donor()), 15, "ready after partition tests")
    print(f"PASS two concurrent two-GPU leases: {sorted(assignments[0])}, {sorted(assignments[1])}")


def revoke():
    mark("1 revocation during a live lease")
    lease = begin("revoke")
    p = running(lease)
    assert lease_container(lease)
    def refusals():
        p = run(["docker", "logs", LINK], quiet=True)
        return p.stderr.count("donor key refused")
    before = refusals()
    run(HUB + ["revoke", DONOR])
    poll(lambda: not lease_container(lease), 12, "revoked container gone")
    p.communicate(timeout=12)
    assert p.returncode != 0
    lost = json.loads(run(DRIVER + ["wait", lease], expected=1).stdout)
    assert not lost["verified"] and lost["facts"]["end"] == "lost" and "revoked" in lost["error"]
    assert donor()["revoked"] and not donor()["connected"]
    assert poll(lambda: refusals() > before, 22, "revoked key refusal")
    print("PASS revocation tears down live lease and refuses the old key", flush=True)


def interrupted(*_):
    raise SystemExit(124)


signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGINT, interrupted)
try:
    if MODE == ["revoke"]:
        revoke()
    else:
        negative_inputs()
        pause()
        heartbeat()
        if os.uname().machine == "aarch64":
            cuda()
        else:
            gpu_partition()
finally:
    for p in children:
        stop(p)
