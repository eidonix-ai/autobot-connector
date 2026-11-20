#!/bin/bash
set -Eeuo pipefail
if (( $# < 4 )); then
    echo "usage: $0 DONOR_ID ARCH MANIFEST PYTEST_PATH [PYTEST_PATH ...]" >&2
    exit 2
fi
donor=$1
arch=$2
manifest=$3
shift 3
[[ $manifest = /* ]] || manifest=$PWD/$manifest
test -f "$manifest"
cd "$(dirname "$0")/.."
mark() { echo "$*" >"smoke-$arch.progress"; }
trap 'mark "exit $?"' EXIT
trap 'exit 124' TERM INT
wait_ready() {
    for _ in {1..40}; do
        if ./hub-cli donors | jq -e --arg id "$donor" '.[] | select(.id == $id and .connected and .status.ready)' >/dev/null; then
            return 0
        fi
        sleep .25
    done
    return 1
}
mark "run the selected Flare CUDA tests"
rc=0
wait_ready
./hub-cli run "$donor" "$manifest" -- /bin/bash -euc '
    set -o pipefail
    mkdir -p /work/tmp
    git -C /work/source rev-parse HEAD | tee /work/test-output
    cd /work/source
    FLARE_RUN_CUDA_TESTS=1 pytest -q -rA -p no:cacheprovider "$@" 2>&1 | tee -a /work/test-output
' bash "$@" | tee "pytest-$arch.log" || rc=$?
mark "verify offline Cargo with Flare Selene geometry tests"
wait_ready
./hub-cli run "$donor" "$manifest" -- /bin/bash -euc '
    set -o pipefail
    mkdir -p /work/tmp /work/cargo
    cp -a /opt/cargo/registry /work/cargo/
    cd /work/source
    CARGO_HOME=/work/cargo CARGO_TARGET_DIR=/work/target \
        cargo test --locked --offline --manifest-path selene/Cargo.toml --lib geometry::tests 2>&1 | tee /work/test-output
' | tee "cargo-$arch.log" || rc=$?
exit "$rc"
