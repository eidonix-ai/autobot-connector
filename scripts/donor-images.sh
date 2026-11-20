# Sourced by the acceptance and deployment scripts.
copy_elf() {
    local target=$1 file=$2 dep
    shift 2
    install -D -m 755 "$file" "$target/${1:-$file}"
    while IFS= read -r dep; do
        install -D -m 755 "$dep" "$target/$dep"
    done < <(ldd "$file" | awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}')
}

build_images() {
    case $(uname -m) in
        aarch64)
            platform=linux/arm64
            libdir=aarch64-linux-gnu
            minor=1
            gpu_model=gb10
            gpu_count=1
            checksum=fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49
            ;;
        x86_64)
            platform=linux/amd64
            libdir=x86_64-linux-gnu
            minor=0
            gpu_model=rtx-pro-6000
            gpu_count=4
            checksum=5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053
            ;;
        *) return 2 ;;
    esac
    printf '%s  %s\n' "$checksum" go.tgz | sha256sum -c
    tar xf go.tgz -C "$build"
    export GOTOOLCHAIN=local CGO_ENABLED=0 GOCACHE="$build/go-cache" GOFLAGS=-modcacherw
    go="$build/go/bin/go"
    if [[ -d $root/module-cache ]]; then
        export GOPROXY="file://$root/module-cache" GOSUMDB=off GOMODCACHE="$build/modules"
    fi
    "$go" version
    "$go" test -v ./...
    "$go" vet ./...


    python3 scripts/donor-cdi.py "$build/cdi.json" "$gpu_count"
    mkdir -p "$build/work/etc" "$build/runner/etc"
    for target in "$build/work" "$build/runner"; do
        printf '%s\n' /usr/lib/$libdir >"$target/etc/ld.so.conf"
        copy_elf "$target" /usr/bin/nvidia-smi
    done
    for cmd in sh sleep cat dd touch rm; do
        copy_elf "$build/work" "$(command -v "$cmd")" "/bin/$cmd"
    done
    if (( with_link )) && [[ -z ${DONOR_WORK_PROFILE:-} ]]; then
        for cmd in git sha256sum tee grep; do
            copy_elf "$build/work" "$(command -v "$cmd")" "/usr/bin/$cmd"
        done
    fi

    if [[ -n ${CUDA_TEST_IMAGE:-} ]]; then
        compiler_name=flare-donor-compiler-$$
        docker run --rm --name "$compiler_name" --pull never --runtime runc --network none \
            --user "$(id -u):$(id -g)" -e TMPDIR=/tmp -e NVIDIA_VISIBLE_DEVICES=void -v "$root:/src:ro" -v "$build:/out" \
            --entrypoint /bin/sh "$CUDA_TEST_IMAGE" -ec '
                nvcc --version
                nvcc -arch=sm_12"$1" -DGPU_MINOR="$1" --cudart=static -O2 -o /out/cuda /src/work.cu
                cuobjdump --list-elf /out/cuda
                cp -L /usr/local/cuda/include/nvml.h /out/nvml.h
            ' sh "$minor"
        nvml_include=$build
    else
        /usr/local/cuda/bin/nvcc --version
        /usr/local/cuda/bin/nvcc -arch=sm_12"$minor" -DGPU_MINOR="$minor" --cudart=static -O2 -o "$build/cuda" work.cu
        /usr/local/cuda/bin/cuobjdump --list-elf "$build/cuda"
        nvml_include=/usr/local/cuda/include
    fi
    if [[ ${DONOR_WORK_PROFILE:-} != flare ]]; then
        copy_elf "$build/work" "$build/cuda" /cuda
    fi
    /bin/cp "$build/cdi.json" "$root/cdi-profile.log"
    pin=$(sha256sum "$build/cdi.json" | cut -d ' ' -f1)
    "$go" build -trimpath -o "$build/work/stage1" ./cmd/stage1
    if [[ ${DONOR_WORK_PROFILE:-} == flare ]]; then
        docker build --pull=false -t flare-donor-work:local -f work-image/connector.Dockerfile "$build/work"
        WORK_IMAGE=$(docker image inspect -f '{{.Id}}' flare-donor-work:local)
    else
        WORK_IMAGE=$(tar -C "$build/work" -cf - . | docker image import --change "ENV LD_LIBRARY_PATH=/usr/lib/$libdir" -)
    fi
    echo "$WORK_IMAGE" >>"$images"
    "$go" build -trimpath -ldflags "-X main.workImage=$WORK_IMAGE -X main.cdiDigest=$pin -X main.gpuModel=$gpu_model -X main.workProfile=${DONOR_WORK_PROFILE:-}" -o "$build/runner/stage1" ./cmd/stage1
    RUNNER_IMAGE=$(tar -C "$build/runner" -cf - . | docker image import --change "ENV LD_LIBRARY_PATH=/usr/lib/$libdir" -)
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
        gcc -Wall -Wextra -Werror -O2 -I"$nvml_include" -o "$build/nvml" native/nvml.c -lnvidia-ml
        mkdir -p "$build/link/link-state"
        chmod 700 "$build/link/link-state"
        copy_elf "$build/link" /usr/bin/git
        copy_elf "$build/link" "$(git --exec-path)/git-remote-http"
        install -D -m 644 /etc/ssl/certs/ca-certificates.crt "$build/link/etc/ssl/certs/ca-certificates.crt"
        copy_elf "$build/link" "$build/nvml" /nvml
        rm -f "$build/link/usr/lib/$libdir/libnvidia-ml.so.1"
        "$go" build -trimpath -o "$build/link/link" ./cmd/link
        "$go" build -trimpath -o "$build/hub" ./cmd/hub
        "$go" build -trimpath -o "$build/driver" ./scripts/driver
        if [[ ${BUILD_HUB_IMAGE:-0} == 1 ]]; then
            mkdir "$build/hub-image"
            install -m 755 "$build/hub" "$build/hub-image/hub"
            HUB_IMAGE=$(tar -C "$build/hub-image" -cf - . | docker image import -)
            echo "$HUB_IMAGE" >>"$images"
        fi
        LINK_IMAGE=$(tar --owner=1000 --group=1000 -C "$build/link" -cf - . | docker image import --change "ENV LD_LIBRARY_PATH=/usr/lib/$libdir" -)
        echo "$LINK_IMAGE" >>"$images"

        export LINK_IMAGE
    fi
    local image
    while read -r image; do
        docker image inspect -f '{{.Id}} {{.Architecture}} {{.Size}}' "$image"
    done <"$images"
}
