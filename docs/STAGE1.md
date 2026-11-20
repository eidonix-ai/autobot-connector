# Stage 1 acceptance run

Run these commands on the DGX Spark test machine from a copy of the source tree. Place the
official Linux arm64 Go 1.26.5 archive beside `go.mod` as `go.tgz`. The script verifies its
checksum and builds both images natively from local files. It uses the installed CUDA 13.0
compiler to build the SM121 kernel. Images are imported locally and selected by their immutable
image IDs. [Go release archive](https://go.dev/dl/#go1.26.5)

```sh
setsid nohup timeout -k 10s 8m bash scripts/run-stage1.sh >stage1.log 2>&1 </dev/null &
```

Watch `progress` and read `stage1.log` for commands and results. The script requires a dedicated
Docker daemon, an idle GPU and passwordless sudo for the host checks. It restarts Docker once and
cleans up the recorded container, image and volume IDs on exit.

The prototype job has one CPU of quota, 512 MiB of memory with zero swap, 48 tasks, and tmpfs
limits of 16 MiB for `/work` and 8 MiB each for `/tmp` and `/dev/shm`. Each has 128 inodes.
The runner checks the live mounts, devices, security profiles and cgroup limits before returning
a lease to the fixture. The local CDI spec pins four CUDA device nodes and five read-only
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
