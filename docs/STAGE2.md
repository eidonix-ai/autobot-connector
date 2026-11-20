# Stage 2 prototype

The Compose link connects to the hub over a pinned TLS WebSocket. The hub exposes its
administrative API on a local Unix socket. The runner keeps Docker authority and the link
has read-only access to the runner's socket volume. Only the link has outbound network access.

On a supported dedicated GPU host, use a source copy with the native Go archive described in
[Stage 1](STAGE1.md). Populate `module-cache` with the module files from Go's download cache
for the version pinned in `go.mod`, preserving their paths under `cache/download`. The script
uses that cache offline. It builds the images natively, starts the test hub with a disposable
certificate, and removes the test resources when it exits.

```sh
setsid nohup timeout -k 15s 14m bash scripts/run-stage1.sh stage2 >stage2.log 2>&1 </dev/null &
```

Watch `progress`. The script runs the full acceptance suite through the link and restarts Docker once. The DGX Spark
run exercises the host memory stop. The discrete GPU run verifies two concurrent leases with
disjoint GPU sets. It writes output to the run log and retains the
hub's per-lease records in `lease-audit`. Run the same launcher with `stage2-health` to exercise
injected readings against live job containers with an injected clock.

With a live stack, stream a single command and receive the signed result through the hub CLI.

```sh
hub code | docker compose exec -T link /link enroll
hub donors
hub run "$donor" manifests/flare.json -- /bin/sh -ec 'git -C source rev-parse HEAD | tee input-checks'
hub revoke "$donor"
```

Use `/link pause` inside the link container to let the current lease finish and refuse new ones.
`/link resume` resumes leasing once the runner and health checks are ready. Use `/link clear` to
clear fan and GPU error latches after checking the machine. A new key requires a new state
volume and a fresh enrollment code.

The GB10 profile requires temperature, power draw, graphics clock and clock event reasons.
It reports power limit, memory clock, memory capacity, fan, ECC and Xid when available.
The RTX PRO 6000 profile also requires the power limit, memory clock, fan speed, and total and
used VRAM for each GPU. It reports ECC and Xid when the driver exposes them. Host-memory
stops and the host-memory recovery floor apply to the GB10. The discrete profile checks assigned
GPU compute processes and VRAM recovery before device reuse. A signed result's `ready` fact
records the runner's teardown and recovery checks for that lease, including the host-memory
floor on GB10. Other leases on discrete cards can continue while those checks run.

The connector pins the native work image and accepts a manifest of SHA-256-pinned files
plus one shallow GitHub checkout pinned by commit. The manifest can request one to four
GPUs. Omitting the count requests one GPU.
