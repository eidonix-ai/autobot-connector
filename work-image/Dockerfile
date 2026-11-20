ARG CUDA_IMAGE=nvidia/cuda:13.2.0-devel-ubuntu24.04
FROM ${CUDA_IMAGE}
ARG TARGETARCH
ARG RUST_VERSION=1.95.0
ENV VIRTUAL_ENV=/opt/flare-venv UV_PROJECT_ENVIRONMENT=/opt/flare-venv \
    CARGO_HOME=/opt/cargo RUSTUP_HOME=/opt/rustup
ENV PATH=/opt/flare-venv/bin:/opt/cargo/bin:${PATH} \
    CARGO_NET_OFFLINE=true PYTHONDONTWRITEBYTECODE=1 \
    CUDA_HOME=/usr/local/cuda \
    LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/lib/aarch64-linux-gnu:/usr/lib/x86_64-linux-gnu
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl g++ git libssl-dev make pkg-config python3.12 python3.12-venv && \
    rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in \
        amd64) target=x86_64-unknown-linux-gnu; \
            rustup_sha=4acc9acc76d5079515b46346a485974457b5a79893cfb01112423c89aeb5aa10; \
            uv_sha=0f4088a04ac92e4c52b4b76759d227a1047355e0ce1dd57cd738a6dec5966bd9 ;; \
        arm64) target=aarch64-unknown-linux-gnu; \
            rustup_sha=9732d6c5e2a098d3521fca8145d826ae0aaa067ef2385ead08e6feac88fa5792; \
            uv_sha=321580b9a7069d0cdbd8db9482a5fb62b4f1285110f847746e3b495408e3a08c ;; \
        *) exit 1 ;; \
    esac; \
    cd "$(mktemp -d)"; \
    curl -fsSLo rustup-init "https://static.rust-lang.org/rustup/archive/1.29.0/$target/rustup-init"; \
    curl -fsSLo uv.tar.gz "https://github.com/astral-sh/uv/releases/download/0.11.27/uv-$target.tar.gz"; \
    printf '%s  %s\n' "$rustup_sha" rustup-init "$uv_sha" uv.tar.gz | sha256sum -c -; \
    chmod +x rustup-init; \
    ./rustup-init -y --profile minimal --default-toolchain "${RUST_VERSION}" --no-modify-path; \
    tar -xzf uv.tar.gz; \
    install -m 755 "uv-$target/uv" "uv-$target/uvx" /usr/local/bin/; \
    download_dir=$PWD; cd /; rm -rf "$download_dir"
RUN --mount=type=bind,source=source,target=/source,rw \
    cd /source && \
    find . -name Cargo.lock -exec sh -ec 'for lock; do CARGO_NET_OFFLINE=false cargo fetch --locked --manifest-path "${lock%/*}/Cargo.toml" || exit; done' sh {} + && \
    CARGO_NET_OFFLINE=false UV_NO_CACHE=1 uv sync --frozen --python /usr/bin/python3.12 \
        --extra prepare --extra test --extra api-test --no-install-project --no-install-package flare-heliopause && \
    chmod -R a+rX /opt/cargo /opt/rustup /opt/flare-venv
ENV HOME=/work TMPDIR=/work/tmp
