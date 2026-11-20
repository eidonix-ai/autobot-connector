#!/bin/bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
root=$PWD
builder=flare-donor-work-$$
tag=flare-donor-toolchain:6555f939
kit=moby/buildkit@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c
if docker image inspect "$tag" >/dev/null 2>&1; then exit 1; fi
kit_present=0
if docker image inspect "$kit" >/dev/null 2>&1; then kit_present=1; fi
build=$(mktemp -d "$root/.flare-build.XXXXXXXX")
keep=0
mark() { echo "$*" >"$root/build-flare.progress"; }
cleanup() {
    local rc=$?
    if docker buildx inspect "$builder" >/dev/null 2>&1; then
        docker buildx rm "$builder" || rc=1
    fi
    if (( !kit_present )) && docker image inspect "$kit" >/dev/null 2>&1; then
        docker image rm "$kit" || rc=1
    fi
    rm -rf "$build"
    if (( !keep )) && docker image inspect "$tag" >/dev/null 2>&1; then
        docker image rm "$tag" || rc=1
    fi
    mark "exit $rc"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 124' TERM INT
case $(uname -m) in
    aarch64) platform=linux/arm64; cuda=13.0.2; digest=450d11555d20ac8ebbbc13ebf17589c2bd42869171a90179ce7098b4a5e64c6a ;;
    x86_64) platform=linux/amd64; cuda=13.2.0; digest=d266e59b88c295bc5fa0e4cef9064eaff84939381b09e2c3d76a5532a303e42d ;;
    *) exit 2 ;;
esac
revision=6555f9397bd7f39b02b836b7f51ce050a54f26a6
mark "fetch Flare $revision"
git init -q "$build/source"
git -C "$build/source" fetch --depth=1 https://github.com/NVIDIA/hyperflare-inference.git "$revision"
git -C "$build/source" checkout --detach FETCH_HEAD
test "$(git -C "$build/source" rev-parse HEAD)" = "$revision"
cp work-image/Dockerfile "$build/Dockerfile"
mark "build $platform with CUDA $cuda"
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$kit"
time docker buildx build --builder "$builder" --platform "$platform" --progress plain --load \
    --build-arg "CUDA_IMAGE=nvidia/cuda:$cuda-devel-ubuntu24.04@sha256:$digest" --tag "$tag" "$build"
docker image inspect -f '{{.Id}} {{.Architecture}} {{.Size}}' "$tag"
keep=1
