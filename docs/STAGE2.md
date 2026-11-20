# Stage 2 prototype

The Compose link connects to the hub over a pinned TLS WebSocket. The hub exposes its
administrative API on a local Unix socket. The runner keeps Docker authority and the link
has read-only access to the runner's socket volume. Only the link has outbound network access.

On the dedicated DGX Spark test machine, use a source copy with the Go archive described in
[Stage 1](STAGE1.md). Populate `module-cache` with the module files from Go's download cache
for the version pinned in `go.mod`, preserving their paths under `cache/download`. The script
uses that cache offline. It builds the images natively, starts the test hub with a disposable
certificate, and removes the test resources when it exits.

```sh
setsid nohup timeout -k 15s 14m bash scripts/run-stage1.sh stage2 >stage2.log 2>&1 </dev/null &
```

Watch `progress`. The script runs the full harness through the link, including a host memory
pressure test and one Docker daemon restart. It writes output to the run log and retains the
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
The prototype pins the GPU and work image and accepts a manifest of SHA-256-pinned files
plus one shallow GitHub checkout pinned by commit.
