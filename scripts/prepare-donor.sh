#!/bin/bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
root=$PWD
build=$(mktemp -d "$root/.build.XXXXXXXX")
images="$root/images.log"
cdi_mkdir="$root/cdi-directory.log"
cdi=/etc/cdi/flare-stage1.json
installed=0
keep=0
owned=0
with_link=1
export DONOR_WORK_PROFILE=flare
mark() { printf '%s\n' "$*" >"$root/prepare.progress"; }
cleanup() {
    local rc=$?
    set +e
    if [[ -n ${compiler_name:-} ]] && docker container inspect "$compiler_name" >/dev/null 2>&1; then
        docker rm -f "$compiler_name" || rc=1
    fi
    if (( owned && !keep )); then
        if [[ -s $images ]]; then xargs -r -n 1 docker image rm <"$images" || rc=1; fi
        if (( installed )); then sudo -n rm "$cdi" || rc=1; fi
        if [[ -s $cdi_mkdir ]]; then sudo -n rmdir "$(dirname "$cdi")" || rc=1; fi
    fi
    rm -rf "$build"
    mark "exit $rc"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 124' TERM INT
mark preflight
test "$(nvidia-smi --query-compute-apps=pid --format=csv)" = pid
test -z "$(docker ps -q)"
test -z "$(docker ps -aq --filter label=com.docker.compose.project=flare-donor)"
sudo -n test ! -e "$cdi"
test ! -e donor.env
test ! -e images.log
: >"$cdi_mkdir"
: >"$images"
owned=1
docker image inspect flare-donor-toolchain:6555f939 >/dev/null
if [[ $(uname -m) == x86_64 ]]; then
    CUDA_TEST_IMAGE=flare-donor-toolchain:6555f939
fi
source scripts/donor-images.sh
mark "build deployment images"
time build_images
install -m 755 "$build/hub" "$root/hub-cli"
cat >donor.env <<ENV
COMPOSE_PROFILES=link
RUNNER_IMAGE=$RUNNER_IMAGE
LINK_IMAGE=$LINK_IMAGE
WORK_IMAGE=$WORK_IMAGE
ENV
if [[ -n ${HUB_IMAGE:-} ]]; then printf 'HUB_IMAGE=%s\n' "$HUB_IMAGE" >hub-image.env; fi
keep=1
