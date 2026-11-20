#!/bin/bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
root=$PWD
mode=${1:-checks}
with_link=0
case "$mode" in
    stage2|stage2-revoke) with_link=1 ;;
    checks|stage2-health|reboot-before|reboot-after) ;;
    *) exit 2 ;;
esac
build=$(mktemp -d "$root/.build.XXXXXXXX")
ids="$root/containers.log"
images="$root/images.log"
volumes="$root/volumes.log"
cdi_mkdir="$root/cdi-directory.log"
cdi=/etc/cdi/flare-stage1.json
owned=0
installed=0
runner=
link=
network=
hub_pid=
cleanup() {
    result=$?
    trap - EXIT
    set +e
    if [[ -n $hub_pid ]]; then
        kill -CONT "$hub_pid" 2>/dev/null
        kill -TERM "$hub_pid" 2>/dev/null
        wait "$hub_pid"
        mkdir -p "$root/lease-audit" || result=1
        cp -r "$build/hub-state/leases/." "$root/lease-audit/" || result=1
    fi
    if [[ -n $link ]]; then
        docker inspect -f '{{json .State}}' "$link"
        docker logs "$link"
    fi
    if [[ -n $runner ]]; then
        docker inspect -f '{{json .State}}' "$runner"
        docker logs "$runner"
    fi
    if (( owned )); then
        docker ps -aq --no-trunc --filter label=org.flare.donor.lease >>"$ids" || result=1
        docker ps -aq --no-trunc --filter label=com.docker.compose.project=flare-donor >>"$ids" || result=1
        docker volume ls -q --filter label=com.docker.compose.project=flare-donor >>"$volumes" || result=1
        while read -r id; do
            if docker container inspect "$id" >/dev/null 2>&1; then
                docker rm -f -v "$id" || result=1
            fi
        done < <(sort -u "$ids")
        if [[ -n $network ]]; then docker network rm "$network" || result=1; fi
        sort -u "$volumes" | xargs -r -n 1 docker volume rm || result=1
        sort -u "$images" | xargs -r -n 1 docker image rm || result=1
        docker ps -a --no-trunc
        docker image ls --no-trunc
        docker volume ls
    fi
    if (( installed )); then
        sudo -n rm "$cdi" || result=1
    fi
    if (( owned )) && [[ -s $cdi_mkdir ]]; then
        sudo -n rmdir "$(dirname "$cdi")" || result=1
        : >"$cdi_mkdir"
    fi
    rm -f "$root/reboot-fixture"
    rm -rf "$build" || result=1
    echo "exit $result" >"$root/progress"
    exit "$result"
}
trap cleanup EXIT
trap 'exit 124' TERM INT
mark() { echo "$*" >"$root/progress"; }
set -x

if [[ $mode == reboot-after ]]; then
    test -f reboot-state.log
    owned=1
    installed=1
    mark "5 verify reboot"
    sudo -n python3 -u scripts/checks.py reboot-after
    exit
fi
: >"$ids"
: >"$images"
: >"$volumes"
: >"$cdi_mkdir"
mark preflight
test "$(uname -m)" = aarch64
test -z "$(docker ps -aq)"
test -z "$(docker volume ls -q --filter label=com.docker.compose.project=flare-donor)"
test -z "$(docker network ls -q --filter label=com.docker.compose.project=flare-donor)"
sudo -n test ! -e "$cdi"
test "$(nvidia-smi --query-compute-apps=pid --format=csv)" = pid
owned=1
cat /etc/os-release
uname -rm
docker version
docker compose version
docker info --format '{{.Driver}} cgroup={{.CgroupVersion}} live-restore={{.LiveRestoreEnabled}}'
findmnt -n -o FSTYPE -T /var/lib/containerd
nvidia-ctk --version
nvidia-smi --query-gpu=name,driver_version,memory.used --format=csv
/usr/local/cuda/bin/nvcc --version

mark build
printf '%s  %s\n' fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49 go.tgz | sha256sum -c
tar xf go.tgz -C "$build"
export GOTOOLCHAIN=local CGO_ENABLED=0 GOCACHE="$build/go-cache" GOFLAGS=-modcacherw
go="$build/go/bin/go"
if [[ -d $root/module-cache ]]; then
    export GOPROXY="file://$root/module-cache" GOSUMDB=off GOMODCACHE="$build/modules"
fi
"$go" version
"$go" test -v ./...
"$go" vet ./...

python3 - "$build/cdi.json" <<'PY'
import json, sys, yaml
with open('/run/cdi/nvidia.yaml') as f:
    spec = yaml.safe_load(f)
device = next(d for d in spec['devices'] if d['name'] == '0')
common = spec['containerEdits']
nodes = common['deviceNodes'] + device['containerEdits']['deviceNodes']
keep = {'/dev/nvidia0', '/dev/nvidiactl', '/dev/nvidia-uvm', '/dev/nvidia-uvm-tools'}
nodes = [node for node in nodes if node['path'] in keep]
assert {node['path'] for node in nodes} == keep
mounts = []
libraries = {'libcuda', 'libnvidia-ml', 'libnvidia-gpucomp', 'libnvidia-ptxjitcompiler'}
for original in common['mounts']:
    name = original['containerPath'].rsplit('/', 1)[-1]
    lib = name.partition('.so.')[0]
    if name != 'nvidia-smi' and lib not in libraries:
        continue
    assert 'ro' in original['options']
    mount = dict(original)
    if lib in {'libcuda', 'libnvidia-ml'}:
        mount['containerPath'] = original['containerPath'].rsplit('/', 1)[0] + '/' + lib + '.so.1'
    mounts.append(mount)
assert len(mounts) == 5
spec = {'cdiVersion': spec['cdiVersion'], 'kind': 'flare.local/gpu',
        'devices': [{'name': '0', 'containerEdits': {'deviceNodes': nodes}}],
        'containerEdits': {'mounts': mounts}}
with open(sys.argv[1], 'w') as f:
    json.dump(spec, f, indent=2, sort_keys=True)
PY
mkdir -p "$build/work/etc" "$build/work/usr/lib/aarch64-linux-gnu" "$build/runner/etc" "$build/runner/usr/lib/aarch64-linux-gnu"
copy_elf() {
    local target=$1 file=$2 dep
    shift 2
    install -D -m 755 "$file" "$target/${1:-$file}"
    while IFS= read -r dep; do
        install -D -m 755 "$dep" "$target/$dep"
    done < <(ldd "$file" | awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}')
}
for target in "$build/work" "$build/runner"; do
    printf '%s\n' /usr/lib/aarch64-linux-gnu >"$target/etc/ld.so.conf"
    copy_elf "$target" /usr/bin/nvidia-smi
done
for cmd in sh sleep cat dd touch rm; do
    copy_elf "$build/work" "$(command -v "$cmd")" "/bin/$cmd"
done
if (( with_link )); then
    for cmd in git sha256sum tee grep; do
        copy_elf "$build/work" "$(command -v "$cmd")" "/usr/bin/$cmd"
    done
fi
/usr/local/cuda/bin/nvcc -arch=sm_121 --cudart=static -O2 -o "$build/cuda" work.cu
copy_elf "$build/work" "$build/cuda" /cuda
/usr/local/cuda/bin/cuobjdump --list-elf "$build/cuda"

/bin/cp "$build/cdi.json" "$root/cdi-profile.log"
pin=$(sha256sum "$build/cdi.json" | cut -d ' ' -f1)
"$go" build -trimpath -o "$build/work/stage1" ./cmd/stage1
WORK_IMAGE=$(tar -C "$build/work" -cf - . | docker image import --change 'ENV LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu' -)
echo "$WORK_IMAGE" >>"$images"
"$go" build -trimpath -ldflags "-X main.workImage=$WORK_IMAGE -X main.cdiDigest=$pin" -o "$build/runner/stage1" ./cmd/stage1
RUNNER_IMAGE=$(tar -C "$build/runner" -cf - . | docker image import --change 'ENV LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu' -)
echo "$RUNNER_IMAGE" >>"$images"
export RUNNER_IMAGE WORK_IMAGE
if [[ ! -d $(dirname "$cdi") ]]; then
    sudo -n mkdir "$(dirname "$cdi")"
    echo created >"$cdi_mkdir"
fi
sudo -n install -m 644 "$build/cdi.json" "$cdi"
installed=1
nvidia-ctk cdi list

if (( with_link )); then
    mkdir -m 700 "$build/hub-state"
    mkdir -p "$build/link/link-state"
    chmod 700 "$build/link/link-state"
    copy_elf "$build/link" /usr/bin/git
    copy_elf "$build/link" "$(git --exec-path)/git-remote-http"
    install -D -m 644 /etc/ssl/certs/ca-certificates.crt "$build/link/etc/ssl/certs/ca-certificates.crt"
    gcc -Wall -Wextra -Werror -O2 -I/usr/local/cuda/include -o "$build/nvml" native/nvml.c -lnvidia-ml
    copy_elf "$build/link" "$build/nvml" /nvml
    rm -f "$build/link/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1"
    "$go" build -trimpath -o "$build/link/link" ./cmd/link
    "$go" build -trimpath -o "$build/hub" ./cmd/hub
    "$go" build -trimpath -o "$build/driver" ./scripts/driver
    LINK_IMAGE=$(tar --owner=1000 --group=1000 -C "$build/link" -cf - . | docker image import --change 'ENV LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu' -)
    echo "$LINK_IMAGE" >>"$images"
    gateway=$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' bridge)
    port=46119
    DONOR_HUB="wss://$gateway:$port/donor"
    DONOR_TEST_HUB=1
    HUB_CERTIFICATE="$build/cert.pem"
    HUB_SOCKET="$build/hub.sock"
    openssl req -x509 -newkey ed25519 -nodes -days 1 -subj /CN=donor-prototype -addext "subjectAltName=IP:$gateway" \
        -keyout "$build/key.pem" -out "$HUB_CERTIFICATE"
    export LINK_IMAGE DONOR_HUB DONOR_TEST_HUB HUB_CERTIFICATE HUB_SOCKET
    "$build/hub" serve "$gateway:$port" "$HUB_CERTIFICATE" "$build/key.pem" "$build/hub-state" "$WORK_IMAGE" >"$root/hub.log" 2>&1 &
    hub_pid=$!
    echo "$hub_pid" >"$root/hub-pid.log"
    for _ in {1..50}; do
        [[ -S $HUB_SOCKET ]] && break
        sleep .1
    done
    test -S "$HUB_SOCKET"
    export COMPOSE_PROFILES=link
fi

mark runner
docker compose create
docker compose ps -aq runner | tee -a "$ids"
docker volume ls -q --filter label=com.docker.compose.project=flare-donor | tee -a "$volumes"
if (( with_link )); then
    link=$(docker inspect -f '{{.Id}}' "$(docker compose ps -aq link)")
    echo "$link" >>"$ids"
    network=$(docker network ls -q --no-trunc --filter label=com.docker.compose.project=flare-donor)
    test -n "$network"
    echo "$network" >"$root/network.log"
fi
docker compose start
runner=$(docker inspect -f '{{.Id}}' "$(docker compose ps -aq runner)")
control=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/control"}}{{.Source}}{{end}}{{end}}' "$runner")
if (( with_link )); then
    source scripts/check-stage2.sh
elif [[ $mode == stage2-health ]]; then
    mark "4 injected health rules with real jobs"
    "$go" test -c -o "$build/health.test" ./internal/link
    sudo -n env DONOR_TEST_RUNNER_SOCKET="$control/runner.sock" "$build/health.test" \
        -test.run '^TestHealthStopsJobsAndRecovers$' -test.v
elif [[ $mode == reboot-before ]]; then
    install -m 755 "$build/runner/stage1" "$root/reboot-fixture"
    sudo -n python3 -u scripts/checks.py "$root/reboot-fixture" "$control/runner.sock" "$runner" "$WORK_IMAGE" "$mode"
    trap - EXIT
    rm -rf "$build"
    mark "5 armed for operator reboot"
else
    sudo -n python3 -u scripts/checks.py "$build/runner/stage1" "$control/runner.sock" "$runner" "$WORK_IMAGE"
fi
