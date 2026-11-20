# Flare test donors

Use dedicated Linux GPU hosts with Docker, Compose, cgroup v2, AppArmor, and the
NVIDIA Container Toolkit's generated CDI specification. The native profiles
cover the arm64 DGX Spark GB10 (SM121) and the amd64 four-card RTX PRO 6000
Blackwell Max-Q workstation (SM120). The work image uses CUDA 13.0.2 on the
R580 driver and CUDA 13.2.0 on the R595 driver. [CUDA driver requirements](https://docs.nvidia.com/cuda/archive/13.2.0/cuda-toolkit-release-notes/index.html)

The runner checks the pinned image and the live job boundary before loading
inputs. The job has an offline network namespace, a read-only root, four CPUs,
24 GiB of RAM with zero swap, 256 tasks, and 16 GiB of writable work tmpfs.
See [Runner acceptance](STAGE1.md) for both work profiles and their full limits.
A manifest can include `"gpus": 2`. Omit the field for one GPU. The runner
reserves the requested number of idle whole GPUs by UUID, gives the job only
those CDI devices, and waits for their recovery before reuse. The signed result
records the assigned UUIDs. Use matching runner, link, and hub versions.

## Build the images and start a donor

1. Check that the machine is idle with
   `nvidia-smi --query-compute-apps=pid --format=csv,noheader` and `docker ps`.
   Place a checkout of the connector in the donor's deployment directory. Supply
   the native Go 1.26.5 archive as `go.tgz` and the Go module download cache as
   described in [Runner acceptance](STAGE1.md).
2. Run the toolchain build. It fetches Flare commit
   `6555f9397bd7f39b02b836b7f51ce050a54f26a6`, runs `uv sync --frozen` for
   `prepare`, `test`, and `api-test` dependencies, and fetches every Cargo
   lockfile's registry packages with `cargo fetch --locked`. The job reads
   Flare's source from its manifest. Python's in-tree `ignition` package is
   imported from that checkout. Workloads that need the in-tree
   `flare_heliopause` extension must build it from the job's source.

   ```sh
   setsid nohup timeout -k 30s 55m bash scripts/build-flare.sh >build-flare.log 2>&1 </dev/null &
   ```

3. Check that `build-flare.progress` reports `exit 0`. Build the donor
   images, pin the restricted CDI profile in `/etc/cdi/flare-stage1.json`, and
   write `donor.env`. The script installs the local `hub-cli` binary and logs
   Docker image IDs and sizes. Set `BUILD_HUB_IMAGE=1` to also build the hub
   image and write its image ID to `hub-image.env`.

   ```sh
   setsid nohup timeout -k 20s 15m bash scripts/prepare-donor.sh >prepare.log 2>&1 </dev/null &
   ```

4. Check `prepare.progress` for `exit 0`. Copy the hub's public, self-signed
   certificate to `hub.pem` with mode 0644. Replace the placeholders below
   with the hub IP or DNS name (`HUB_ADDRESS`), the TCP listener port
   (`HUB_PORT`), and the absolute path to the donor's connector checkout
   (`DONOR_DIRECTORY`). Use brackets around an IPv6 address in the URL.
   Add `DONOR_TEST_HUB=1` to `donor.env` only for a hub on a private network.

   ```dotenv
   DONOR_HUB=wss://HUB_ADDRESS:HUB_PORT/donor
   HUB_CERTIFICATE=DONOR_DIRECTORY/hub.pem
   ```

   Run `docker compose --env-file donor.env up -d`. Feed a fresh hub code to
   `docker compose --env-file donor.env exec -T link /link enroll` on stdin.
   Both services use `restart: unless-stopped`. Use
   `docker compose --env-file donor.env ps` to check the services and
   the hub's `donors` command to check enrollment, health, and available GPU count.

## Configure the Compose hub

Run the following commands from the connector checkout on the hub host. The
operator creates `hub.env` here. Use the `HUB_IMAGE` ID from `hub-image.env`
when the hub image was built locally. To transfer a hub image between hosts of
the same architecture, give that ID to `docker image save -o hub-image.tar`,
transfer the archive, then run `docker image load -i hub-image.tar`. For a
different architecture, build and import the hub on the destination and use
the image ID printed by the import.

```sh
mkdir -p hub-image
CGO_ENABLED=0 go build -trimpath -o hub-image/hub ./cmd/hub
tar -C hub-image -cf - . | docker image import -
```

Set these shell variables before generating the certificate and environment file.

| Variable | Value |
| --- | --- |
| `HUB_LISTEN` | The local bind IP and operator-selected TCP port, in `IP:port` form. Enclose an IPv6 bind address in brackets. Allow donors to reach this port. |
| `HUB_SAN` | `IP:HUB_ADDRESS` or `DNS:HUB_ADDRESS`, with `HUB_ADDRESS` replaced by the address or name donors use, without a port or IPv6 brackets. |
| `HUB_IMAGE` | The hub's local Docker image ID. |
| `HUB_WORK_IMAGES` | A comma-separated list of `architecture=image-ID` pins. Use `amd64` or `arm64` and the matching donor's `WORK_IMAGE` from `donor.env`. Include one pin per deployed architecture. |

Use a 90-day certificate to limit how long a copied private key can
authenticate the hub. Schedule rotation by day 75 to allow time for
maintenance delays. The hub directory owner also owns the admin socket.

```sh
umask 077
mkdir -m 700 hub-data
openssl req -x509 -newkey ed25519 -nodes -days 90 -subj /CN=flare-donor-hub \
  -addext "subjectAltName=$HUB_SAN" -keyout hub-data/tls.key -out hub-data/tls.pem
cat >hub.env <<ENV
HUB_IMAGE=$HUB_IMAGE
HUB_LISTEN=$HUB_LISTEN
HUB_WORK_IMAGES=$HUB_WORK_IMAGES
HUB_UID=$(id -u)
HUB_GID=$(id -g)
HUB_DIRECTORY=$PWD/hub-data
ENV
docker compose --env-file hub.env -f compose.hub.yml up -d
docker compose --env-file hub.env -f compose.hub.yml exec -T hub /hub donors
```

Generate a distinct enrollment code for each donor with
`docker compose --env-file hub.env -f compose.hub.yml exec -T hub /hub code`.
Pass each code on stdin to its donor's
`docker compose --env-file donor.env exec -T link /link enroll`.

## Run the Flare smoke tests

On the hub host, install `jq` and build the CLI with
`CGO_ENABLED=0 go build -trimpath -o hub-cli ./cmd/hub`. Export
`HUB_SOCKET="$PWD/hub-data/hub.sock"` from the hub's deployment directory.
Run `bash scripts/smoke-flare.sh DONOR_ID ARCH MANIFEST PYTEST_PATH`, supplying
the donor ID from `./hub-cli donors`, its architecture (`amd64` or `arm64`),
the path to a manifest with a commit-pinned Flare checkout, and at least one
pytest path or node ID in that checkout. Use `"salvage": ["test-output"]` in
the manifest to retrieve the test output.

The script enables Flare's CUDA tests for the selected pytest paths, then runs
the Selene geometry tests with `cargo test --locked --offline` in a separate
lease. It writes both CLI results to architecture-specific logs. The hub
stores signed results under `hub-data/state/leases/`. Confirm the selected
CUDA tests passed without skipping and both jobs have a zero exit code and
`"verified": true`.

## Drain donors for hub maintenance

From `DONOR_DIRECTORY` on each donor, run
`docker compose --env-file donor.env exec -T link /link pause`. Wait for
submitted leases to finish and for
`docker ps -q --filter label=org.flare.donor.lease` to print nothing on every
donor. Then stop each link with `docker compose --env-file donor.env stop link`.
Run hub Compose commands from the hub host's connector checkout.

## Rotate the certificate on the same hub

1. Generate a replacement key and certificate in a private staging directory
   using the certificate command above with the hub's `HUB_SAN`. Check the SAN,
   expiry, and SHA-256 fingerprint with
   `openssl x509 -in tls.pem -noout -ext subjectAltName -enddate -fingerprint -sha256`.
2. Drain the donors and stop the hub with
   `docker compose --env-file hub.env -f compose.hub.yml stop hub`. Replace
   `hub-data/tls.key` and `hub-data/tls.pem` with the staged pair. Keep the
   private key readable only by the hub directory owner. Start the hub using
   the existing `hub.env` and `hub-data/state`.
3. Transfer the public PEM to each donor, verify its fingerprint over a trusted
   channel, and install it at `HUB_CERTIFICATE` with mode 0644. Recreate each
   link with `docker compose --env-file donor.env up -d --no-deps --force-recreate link`
   so it opens the replacement certificate. Check that the existing donor IDs
   reconnect. Resume each link with
   `docker compose --env-file donor.env exec -T link /link resume`, then check
   readiness and run a manifest-pinned smoke test.

## Move the hub to another server

1. Drain the donors. Record their IDs and their architecture-specific
   `WORK_IMAGE` pins. On the source hub, revoke each ID with
   `docker compose --env-file hub.env -f compose.hub.yml exec -T hub /hub revoke DONOR_ID`.
   Stop the source hub and archive `hub-data/state/leases` if you need its
   signed results.
2. Place a connector checkout on the destination and configure a fresh Compose
   hub as described above using the recorded image pins and the destination
   listener. The fresh hub has an empty donor registry.
3. Verify and install the destination's public certificate on each donor as
   in the rotation procedure. Update `DONOR_HUB` and the private-network
   setting in each `donor.env`. Reset each stopped link's identity in its
   existing volume and start it against the destination.

   ```sh
   docker compose --env-file donor.env run --rm --no-deps link /link reset-enrollment
   docker compose --env-file donor.env up -d --no-deps --force-recreate link
   ```

4. Enroll each donor with a distinct code from the destination hub. Check that
   the new IDs appear as connected. Resume the links, check readiness, and
   submit a manifest-pinned test before pointing the harness at the destination
   hub's Unix socket.
