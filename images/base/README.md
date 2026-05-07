# FORGE base image

This directory builds the FORGE thin base VM image: Debian 12 (bookworm) arm64
with `openssh-server`, `cloud-init`, basic tooling, and a `forge-ready`
systemd service that signals boot completion via virtio-vsock.

The image deliberately does **not** include k3s, RAGE, or Claude Code — those
are installed at `forge env create` time via cloud-init at versions pinned in
the project's `forge.yaml`.

## Contents

```
images/base/
├── build.sh                    Main build script (virt-customize)
├── build-in-docker.sh          Wraps build.sh in a container — for macOS hosts
├── Dockerfile                  Builder image: Debian + libguestfs + qemu + syft
├── publish.sh                  Publishes artefacts to GitHub Releases
├── files/
│   ├── forge-ready.service     systemd unit, runs after cloud-final
│   ├── forge-ready.sh          Sends `ready addr=<ip>` over vsock CID 2:1234
│   └── forge-vsock.conf        Loads vsock kernel modules at boot
└── README.md                   This file
```

## Two ways to build

### On a Linux host (CI and Linux developers)

`build.sh` shells out to `virt-customize` directly. Install:

```sh
sudo apt install -y libguestfs-tools qemu-utils curl gzip
curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh \
    | sudo sh -s -- -b /usr/local/bin
```

Then `VERSION=0.1.0 ./build.sh`. Output is always arm64 regardless of
host architecture.

### On macOS / any host with Docker (local development)

`build-in-docker.sh` runs the same `build.sh` inside a Linux container
that has libguestfs and syft preinstalled — no host-side toolchain
required beyond Docker:

```sh
VERSION=dev ./build-in-docker.sh
forge image import output/forge-base-dev-arm64.img.gz
```

The container needs `--privileged` (libguestfs spawns its own appliance
VM), and on macOS it falls back to QEMU TCG emulation since nested KVM
isn't reliably available — expect builds in the 10–20 minute range on
Apple Silicon. Both scripts produce identical artefacts.

## Building

```sh
VERSION=0.1.0 ./build.sh
```

Outputs (in `./output/`):
- `forge-base-0.1.0-arm64.img.gz`
- `sbom.spdx.json`
- `sbom.cdx.json`
- `SHA256SUMS`

### Targeting a different Debian release

The build defaults to Debian 13 (trixie). To target a different major
version, just set `DEBIAN_VERSION` — the codename is looked up
automatically from Debian's [`distro-info-data`](https://salsa.debian.org/debian/distro-info-data):

```sh
VERSION=0.1.0 DEBIAN_VERSION=12 ./build.sh   # bookworm
VERSION=0.1.0 DEBIAN_VERSION=14 ./build.sh   # forky (whenever)
```

The CSV is cached under `./build/` after the first run so subsequent
builds work offline. Set `DEBIAN_RELEASE` explicitly if you want to
pin to a non-stable suite (e.g. `sid`) or skip the lookup.

| Variable | Default | Purpose |
|----------|---------|---------|
| `VERSION` | `dev` | Tag baked into output filenames |
| `ARCH` | `arm64` | Target architecture (only `arm64` supported on macOS hosts today) |
| `DEBIAN_VERSION` | `13` | Debian major version |
| `DEBIAN_RELEASE` | _(auto)_ | Override the upstream codename lookup |
| `WORK_DIR` | `./build` | Scratch space for downloads / intermediates |
| `OUTPUT_DIR` | `./output` | Where final artefacts land |

## Publishing

```sh
VERSION=0.1.0 ./publish.sh
```

Requires `gh` CLI authenticated against `schubergphilis/forge` (or whatever
repo `REPO` is set to).

## Verifying the image boots correctly

The `forge-ready` service listens-on / sends-to vsock CID 2 port 1234 once
cloud-init finishes. To smoke-test on macOS Apple Silicon with vfkit:

```sh
# in one terminal — listen for the vsock signal
socat - VSOCK-LISTEN:1234

# in another — boot the image
vfkit \
    --cpus 2 --memory 2048 \
    --bootloader efi,variable-store=$PWD/efi-vars,create \
    --device virtio-blk,path=forge-base-*.img \
    --device virtio-vsock,port=1234
```

You should see `ready addr=<vm-ip>` appear in the listener.
