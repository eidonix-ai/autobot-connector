FROM flare-donor-toolchain:6555f939
RUN TMPDIR=/tmp apt-get update && TMPDIR=/tmp apt-get install -y --no-install-recommends protobuf-compiler && \
    rm -rf /var/lib/apt/lists/*
ENV LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu:/usr/lib/x86_64-linux-gnu
COPY stage1 /stage1
