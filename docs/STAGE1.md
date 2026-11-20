# Stage 1 acceptance run

Run these commands on a dedicated DGX Spark or a four-card RTX PRO 6000
Blackwell Max-Q workstation from a copy of the source tree. Put the official
Linux Go 1.26.5 archive for the machine's architecture beside `go.mod` as
`go.tgz`. The script verifies its checksum and builds both images natively.
It compiles the SM121 kernel with the host CUDA compiler on GB10. On a host
without CUDA installed, set `CUDA_TEST_IMAGE` to a locally available native
development image with an SM120-capable compiler. The Go module can be supplied from a local download cache
as described in [Stage 2](STAGE2.md). Images are imported locally and selected by their immutable
image IDs. [Go release archive](https://go.dev/dl/#go1.26.5)

```sh
setsid nohup timeout -k 10s 8m bash scripts/run-stage1.sh >stage1.log 2>&1 </dev/null &
```

Watch `progress` and read `stage1.log` for commands and results. The script requires a dedicated
Docker daemon, an idle GPU and passwordless sudo for the host checks. It restarts Docker once and
cleans up the recorded container, image and volume IDs on exit.

The runner image fixes the work profile at build time. The acceptance script
uses the default profile. `scripts/prepare-donor.sh` builds the `flare` profile.

| Per-lease limit | Default | `flare` |
| --- | --- | --- |
| CPU quota | 1 CPU | 4 CPUs |
| Memory | 512 MiB | 24 GiB |
| Swap | 0 | 0 |
| Tasks | 48 | 256 |
| `/work` tmpfs | 128 MiB, 8,192 inodes | 16 GiB, 524,288 inodes |
| `/tmp` and `/dev/shm` tmpfs, each | 8 MiB, 128 inodes | 8 MiB, 128 inodes |

The runner checks the live mounts, devices, security profiles and cgroup limits before returning
a lease to the fixture. The local CDI spec pins the assigned GPU node, three common CUDA nodes, and five read-only
driver mounts. Docker's named standard system mounts retain their default flags. The trusted
runner uses the host PID namespace so the GPU query can see host compute PIDs. Jobs use private
PID namespaces.

The fixture uses `stage1 fixture SOCKET run LEASE DURATION [SALVAGE_PATH ...]`, followed by
`exec LEASE COMMAND [ARG ...]`, `finish LEASE`, `stop LEASE` or `status`. The runner owns the
deadline. Its final status carries the command exit codes, OOM counters, Docker's `OOMKilled`
field and the bounded salvage archive when available. A fixed exporter reads named paths from
live tmpfs after workload quiescence. Docker's copy operation cannot see these files.
[Docker copy documentation](https://docs.docker.com/reference/cli/docker/container/cp/)

## Operator-run host reboot check

Prepare a fresh stack and leave a 10-minute lease running.

```sh
setsid nohup timeout -k 10s 4m bash scripts/run-stage1.sh reboot-before >reboot-before.log 2>&1 </dev/null &
```

Wait for `5 armed for operator reboot` in `progress`. The fixture has streamed a marker and the
script has recorded the old boot ID, container, cgroup and process identities. Reboot the test
machine while that lease is live. As soon as the machine is reachable, run the verifier.

```sh
setsid nohup timeout -k 10s 3m bash scripts/run-stage1.sh reboot-after >reboot-after.log 2>&1 </dev/null &
```

The verifier compares the boot IDs, measures readiness from Docker's systemd start timestamp,
requires the lease to be marked lost, checks the old label, cgroup and processes, and verifies
that the next lease cannot see any of the three markers. It then removes the recorded resources.
