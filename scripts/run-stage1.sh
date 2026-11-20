#!/bin/bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
root=$PWD
mode=${1:-checks}
case "$mode" in checks|reboot-before|reboot-after) ;; *) exit 2 ;; esac
build=$(mktemp -d "$root/.build.XXXXXXXX")
ids="$root/containers.log"
images="$root/images.log"
volumes="$root/volumes.log"
cdi_mkdir="$root/cdi-directory.log"
cdi=/etc/cdi/flare-stage1.json
owned=0
installed=0
runner=
cleanup() {
    result=$?
    trap - EXIT
    set +e
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
    rm -rf "$build"
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
export GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0 GOCACHE="$build/go-cache"
go="$build/go/bin/go"
"$go" version
"$go" test ./...
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

mark runner
docker compose create
docker compose ps -aq runner | tee -a "$ids"
docker volume ls -q --filter label=com.docker.compose.project=flare-donor | tee -a "$volumes"
docker compose start
runner=$(docker inspect -f '{{.Id}}' "$(docker compose ps -aq runner)")
control=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/control"}}{{.Source}}{{end}}{{end}}' "$runner")
if [[ $mode == reboot-before ]]; then
    install -m 755 "$build/runner/stage1" "$root/reboot-fixture"
    sudo -n python3 -u scripts/checks.py "$root/reboot-fixture" "$control/runner.sock" "$runner" "$WORK_IMAGE" "$mode"
    trap - EXIT
    rm -rf "$build"
    mark "5 armed for operator reboot"
else
    sudo -n python3 -u scripts/checks.py "$build/runner/stage1" "$control/runner.sock" "$runner" "$WORK_IMAGE"
fi
