# Donated hardware

People lend idle GPU machines so Flare's changes get tested on hardware we do not have: other GPU
architectures, more GPUs, other interconnects. A testing agent on our side drives each job. The
donor's own coding agent sets the machine up from a brief. This file is the contract (2026-11-20;
fourth version, after three design reviews).

## 1. Principles

- **The point is hardware coverage.** Each machine reports exactly what it is; each test goes to the
  hardware it needs; each case records what ran where and what was not covered.
- **Simple for donors**: the donor gives their coding agent `SETUP.md` and answers a few questions.
  Stop, start, update and uninstall are documented one-line commands.
- **A fresh, offline container per job.** PR code runs only in a container created for that job, with
  no network, and destroyed after it.
- **Three parts, each with one power**:
  - the **runner** controls Docker (to create and destroy job containers) and has no network;
  - the **link** has the network (to reach our hub) and no Docker;
  - the **job container** has neither.
  The runner's local API to the link is narrow: run a job of a fixed shape, stop it, report. It
  cannot be asked for privileges, mounts, devices or images outside its built-in profile.
- **What the donor trusts**: the runner, like any self-hosted CI runner, holds Docker's authority on
  the machine, which is root-equivalent. It is small, open source, pinned by digest, and never runs
  PR code itself. Donated machines must be dedicated to testing, with no other workloads or
  credentials, and SETUP.md says so first: a GPU job also reaches the host's driver, and a driver bug
  is a path to the host that containers do not close.
- **Nothing a donor returns is trusted without checks**, and no credentials go to donors.

## 2. Setup by the donor's agent

`SETUP.md` (in this repository, at a tagged release) is a brief for the donor's coding agent. It
tells the agent to:

1. Confirm with the donor that the machine is dedicated to donating; stop if not.
2. Check the supported stack listed in the brief (OS, kernel, NVIDIA driver, Docker Engine, Compose,
   NVIDIA container toolkit with CDI, cgroup v2) and report the hardware: GPU models, architecture
   (SM), count, memory, interconnect (NVLink or PCIe topology), CPU, RAM, disk.
3. Show the donor the default limits and accept them in one step, or take changes:

   | Setting | Default |
   |---|---|
   | GPUs (indexes or UUIDs, or none) | all |
   | CPU cores for a job | half |
   | RAM for a job (no swap) | half |
   | Scratch for a job (in RAM for now, counted in the job's RAM) | 32 GB |
   | Hours jobs may run | any time |

4. Fetch the release at the brief's tag, check the compose file's SHA-256 against the brief, and write
   `~/.flare-donor/compose.yml` and `limits.env`, changing only the documented fields. Never add
   privileges, devices, mounts, network settings or capabilities.
5. Enroll with the code the donor pastes from Discord (`/donate`), without echoing it to history or
   logs; start; run the self-check; show the result and the commands (`docker compose -f
   ~/.flare-donor/compose.yml stop|start|logs`, update, and uninstall with `down -v`, removing the
   folder and images, and `/donate revoke`).

Shared models come later (section 9).

## 3. The parts

The compose file (project `flare-donor`) runs the runner and the link. Both images are pinned by
digest; logs are bounded; both have CPU, memory and process caps.

**runner**: our code, no network (`network_mode: none`), the Docker socket mounted, a state volume.
- Listens on a socket in a volume shared only with the link (never with job containers), mode 0700,
  owned by the runner and link's shared uid.
- For each lease: creates a job container from the pinned work image (preloaded by digest) with its
  **built-in profile** (below), labelled with the lease id. The container's main process is a fixed
  **keeper** from the work image (it only holds the container open and reaps children), so `/work`
  lives for the whole lease.
- **Inputs** are streamed through stdin into a fixed unprivileged loader run with `docker exec`,
  which checks the bundle's digest before extracting it into `/work` with byte and file-count caps.
- **Commands** from the testing agent run with `docker exec`, each in its own process group; output is
  streamed to the link as it comes.
- **Deadline**: the runner owns the timer, independent of the link, the fixture and output delivery.
  At the deadline or on a stop it kills every workload process group, leaving only the keeper.
- **Salvage**, once the workload has stopped and while the container still exists: a fixed exporter
  run with `docker exec` streams the named paths out as an archive, within a size cap. Then the runner
  records the exit facts (each command's exit code or signal, the job cgroup's `memory.events`
  OOM-kill count before and after, the container's `OOMKilled`), stops and removes the container, and
  verifies teardown (section 8) before reporting ready.
- Refuses a new lease while any check fails, and reports `unhealthy` with the reason.
- **Recovery**: the runner itself runs under `restart: unless-stopped`. On start, and whenever its
  connection to Docker comes back, it reconciles: every container carrying its lease label is killed
  and removed at once, that lease is reported lost (only what was already streamed survives), and the
  teardown checks run before it is ready. Each failure case has a stated recovery bound in section 8.

**job container** (built-in profile, not configurable by the link or the hub): the work image
(Flare's toolchain, built natively per architecture with code for every SM it is offered on; an image
that declares volumes is refused); `network_mode: none`; read-only root; `/work` as tmpfs with
explicit options (size and `nr_inodes` from the donor's scratch limit, mode 0700, owned by uid 1000,
`nosuid,nodev`); `/tmp` and `/dev/shm` as small tmpfs with the same kind of options; uid 1000, no
capabilities, `no-new-privileges`, Docker's seccomp and AppArmor profiles; the selected GPUs through
CDI; CPU, memory (memory-plus-swap equal to memory) and process caps; no restart policy. Its mounts
are exactly: those tmpfs mounts, the device nodes and driver files the pinned CDI specification
injects for the selected GPUs (read-only where the specification allows), and Docker's standard
system mounts; anything else, or anything writable that is not one of the tmpfs mounts, fails the
profile check.

**link**: our code, network allowed, no Docker, uid 1000, its key in a state volume (mode 0700).
- Connects out over verified TLS to the hub and to a fixed list of public hosts for job inputs
  (GitHub, crates.io's file host, PyPI's file host, Hugging Face), dialling the address it resolved
  and checked (refusing loopback, private, link-local and container networks, except an explicitly
  configured test hub in the prototype), with no redirects outside the list.
- Fetches each file of a job's manifest from its public URL, verifies its digest, and hands verified
  files to the runner through their shared socket volume. Later, a digest-addressed cache owned by the
  link saves repeat downloads; every use re-verifies the digest.
- Enrolls, reports the machine, its limits and its health; relays leases and operations to the runner
  and output back to the hub; treats everything from the runner's job output as hostile.
- Reads GPU health through NVML (section 4).

## 4. Machine health

- Readings per GPU through NVML: temperature, power against its limit, clocks, throttle reasons, fan
  speed where reported, ECC and Xid errors. Each supported hardware type lists its mandatory and
  optional readings; a machine missing a mandatory reading gets no GPU jobs.
- Rules, each with its own trigger and recovery:

  | Signal | Trigger | Recovers when |
  |---|---|---|
  | GPU temperature | at or above 83 °C (or the donor's lower value) | 10 °C below it for 5 minutes |
  | Throttling | thermal or hardware slowdown | none for 5 minutes |
  | Fan (where reported) | zero while the GPU is above 60 °C | the donor clears it |
  | ECC or Xid error | any new one | the donor clears it |
  | Host memory | available under 10% | above 20% for 5 minutes |

  Any trigger stops the running job through the runner, marks the machine `cooling` or `unhealthy`,
  and refuses leases until recovery. When several trigger, the machine recovers only when all have.
- Duty cycle: at most 2 hours of GPU work at a stretch, then 15 minutes' rest.

## 5. A job

1. **Prepare (our side)**: the hub writes a small manifest for the exact head and target, from the
   PR's lockfiles read as data in our own sandbox, with nothing in the PR run and nothing large
   downloaded on our side: the source by commit from GitHub (the commit hash authenticates it), pinned
   submodule commits and LFS objects by their SHA-256, crates from static.crates.io with the lockfile's
   checksums, the complete wheel set for the target's Python ABI and platform from PyPI's file host
   with their hashes, and an offline package-manager configuration. Every URL must be on the link's
   fixed public list; a missing lock, an unsupported source or a missing hash refuses the job. Bulk
   bytes never pass through our hub.
2. **Match**: only to a machine whose hardware, limits and health fit.
3. **Lease**: nonce, lease id, head, image and bundle digests, deadline, scratch need, salvage paths.
4. **Run**: the link fetches and verifies the manifest's files; the runner creates the container and
   streams them in through the loader; the testing agent drives the job.
5. **End**: the runner stops every process, salvages, records the exit facts, destroys the container,
   checks teardown, and reports; the link sends `done` with the signed transcript.

## 6. Logs and crashed jobs

- Everything a job prints is streamed to the hub as it happens.
- At the end of every job, after its workload has stopped and while the container still exists, the
  runner streams out the salvage paths within a size cap.
- Exit facts: exit code or signal per command, `OOMKilled`, the deadline, and the health readings and
  Xid errors around the time. A cause the runner could not establish is reported as unknown.
- If the runner itself dies or the machine reboots mid-job, only the streamed output remains; the
  runner's start-up cleanup removes the old container, and the lease is reported as lost.

## 7. Protocol and trust

- **Enrollment**: single-use Discord code, valid an hour; the link makes an ed25519 key and registers
  it over TLS; reconnects sign a hub challenge; revocation ends the session and any lease; rotation is
  re-enrollment.
- **Messages**: one WebSocket the link opens; a strict state machine; no compression; frames capped
  before decoding; per-lease caps on operations, bytes and time; connection-rate limits; backpressure.
- **Transcripts**: one canonical encoding (nonce, ids, head, digests, every operation and output in
  order, exit facts, health readings), signed; the hub keeps its own record and refuses stale,
  incomplete, reordered or duplicate transcripts.
- **Use of results**: untrusted evidence, escaped in every view, never given to a model as
  instructions. A result that affects a decision is corroborated by an independent runner of
  comparable hardware: ours when we have it, otherwise another donor's. Where no second comparable
  machine exists, the result is recorded as uncorroborated evidence for that hardware and never decides
  alone. Donor host names and paths are scrubbed before anything is public. Only PRs that passed
  intake go to donors.

## 8. Prototype stage 1 on a DGX Spark (GB10, arm64)

Pinned stack: Ubuntu 24.04.5, kernel 7.0.0-1025-nvidia, NVIDIA driver 580.178.04 (CUDA 13.0),
Docker 29.6.2 with live-restore off, Compose 5.2.0, NVIDIA container toolkit 1.20.1 with CDI,
cgroup v2, overlayfs on ext4. The machine is dedicated to this test with no production keys; the
runner and a local fixture standing in for the link run there; no hub, no enrollment, no models.

Each check is recorded with its command and output, and passes only as stated:
1. **Profile**, from the live job process as well as `docker inspect`: its mount table (exactly the
   allowed set of section 3, the CDI driver mounts listed), device nodes, uid, capabilities, seccomp
   and AppArmor state, and cgroup limits (CPU, memory, swap equal to memory, pids) as specified.
2. **Network**: from a job, connections fail to live listeners on the host, on the LAN, in another
   container, over IPv6 and to DNS; for each, a control request of the same address family and
   protocol from the host succeeds; the runner's socket does not exist inside the job.
3. **Limits**: writes fail at the scratch byte cap and at its inode cap; `/tmp` and `/dev/shm` fail at
   their sizes; a fork bomb stops at the pid cap; a command over the memory cap is OOM-killed, seen as
   an increase in the job cgroup's `memory.events` OOM-kill count, while the keeper and the runner
   stay up; CPU throttling under load matches the cap (from the cgroup's `cpu.stat`).
4. **GPU** (the GB10 reports GPU memory use as not supported): a real SM121 CUDA kernel runs as uid
   1000 with a touched allocation of a fixed size; host `MemAvailable` and the job cgroup's memory are
   recorded before, during and after; the host's GPU process query must show the live CUDA process
   during the job before it counts as evidence that the process is gone after; ready requires no job
   process, no job cgroup, no GPU compute process, and host `MemAvailable` back within 1 GB of its
   baseline within 30 seconds. Whether CUDA memory is charged to the job's cgroup is measured and
   written down; a general GPU memory cap is not claimed.
5. **Teardown**, checked from the host before the next lease, for each ending, with its bound: normal
   end, cancel and deadline (ready within 30 seconds); OOM (30 seconds); the fixture killed mid-job
   (the deadline still fires; ready 30 seconds after it); the runner killed mid-job (on its restart,
   the job container is removed and the lease reported lost; ready within 60 seconds); a Docker daemon
   restart (with live-restore off: ready within 2 minutes of the daemon); a host reboot (ready within
   2 minutes of Docker starting). In each: no container with the lease label, no job cgroup, no job
   process, and a second lease finds no marker of the first; output and exit facts arrive where the
   ending allows and are marked lost where it does not.
6. **Refusal**: with a leftover labelled container, or a process holding memory so `MemAvailable`
   stays below the bound, the runner refuses the next lease and reports why.

Later stages: the link and hub (enrollment, state machine, transcripts, injected health readings,
pause as "finish the running job, take no new one"), then a real Flare job from a hub-prepared bundle,
then SETUP.md tried by a fresh agent on a clean machine.

## 9. Later

- **Shared models**: the donor's model folders, mounted read-only into job containers by the runner
  per lease (only the folders the lease needs), with digests checked against a trusted manifest when
  the lease starts.
- **Disk scratch** larger than RAM allows, once a hard-limited disk option is proven on the
  supported stack.
- **Telemetry** from host paths under `/sys`, each named here before it is mounted.
- Per-GPU selection on a multi-GPU machine, amd64 images, and a VM boundary for donors who want it.
