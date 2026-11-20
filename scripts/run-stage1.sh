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
    if [[ -n ${compiler_name:-} ]] && docker container inspect "$compiler_name" >/dev/null 2>&1; then
        docker rm -f "$compiler_name" || result=1
    fi
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

mark build
source scripts/donor-images.sh
build_images

if (( with_link )); then
    mkdir -m 700 "$build/hub-state"
    gateway=$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' bridge)
    port=$(python3 -c 'import socket, sys; s = socket.socket(); s.bind((sys.argv[1], 0)); print(s.getsockname()[1])' "$gateway")
    DONOR_HUB="wss://$gateway:$port/donor"
    DONOR_TEST_HUB=1
    HUB_CERTIFICATE="$build/cert.pem"
    HUB_SOCKET="$build/hub.sock"
    openssl req -x509 -newkey ed25519 -nodes -days 1 -subj /CN=donor-prototype -addext "subjectAltName=IP:$gateway" \
        -keyout "$build/key.pem" -out "$HUB_CERTIFICATE"
    export LINK_IMAGE DONOR_HUB DONOR_TEST_HUB HUB_CERTIFICATE HUB_SOCKET
    "$build/hub" serve "$gateway:$port" "$HUB_CERTIFICATE" "$build/key.pem" "$build/hub-state" "${platform#linux/}=$WORK_IMAGE" >"$root/hub.log" 2>&1 &
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
    sudo -n env DONOR_TEST_GPU_MODEL="$gpu_model" DONOR_TEST_RUNNER_SOCKET="$control/runner.sock" "$build/health.test" \
        -test.run '^(TestHealthStopsJobsAndRecovers|TestDiscreteHostMemoryDoesNotStopJob)$' -test.v
elif [[ $mode == reboot-before ]]; then
    install -m 755 "$build/runner/stage1" "$root/reboot-fixture"
    sudo -n python3 -u scripts/checks.py "$root/reboot-fixture" "$control/runner.sock" "$runner" "$WORK_IMAGE" "$mode"
    trap - EXIT
    rm -rf "$build"
    mark "5 armed for operator reboot"
else
    sudo -n python3 -u scripts/checks.py "$build/runner/stage1" "$control/runner.sock" "$runner" "$WORK_IMAGE"
fi
