#!/bin/bash
# Build the FORGE thin base image inside a Docker container.
#
# Useful when the host doesn't have libguestfs/qemu/syft installed —
# mainly macOS developers iterating locally. Output lands in ./output/
# just like ./build.sh; pipe it into `forge image import` to make the
# new image immediately available to `forge env create`.
#
# Environment variables (all optional, forwarded into the container):
#   VERSION         — image version tag (default: dev)
#   ARCH            — target architecture (default: arm64)
#   DEBIAN_VERSION  — Debian major version (default: 12)
#   DEBIAN_RELEASE  — Debian release codename (default: bookworm)
#
# Examples:
#   ./build-in-docker.sh
#   VERSION=0.2.0-rc1 ./build-in-docker.sh
#   VERSION=trixie-test DEBIAN_VERSION=13 DEBIAN_RELEASE=trixie ./build-in-docker.sh

set -euo pipefail

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "error: required tool not found: $1" >&2
        exit 1
    }
}
require docker

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE_TAG="${IMAGE_TAG:-forge-base-builder:latest}"

mkdir -p "$SCRIPT_DIR/output" "$SCRIPT_DIR/build"

echo "==> Building builder image ($IMAGE_TAG)"
docker build -t "$IMAGE_TAG" "$SCRIPT_DIR"

echo "==> Running build inside container"
# --privileged is required because libguestfs spawns its appliance VM
# via /dev/kvm (when available) or QEMU TCG; either way it needs to
# create loop devices. There is no read-only path that works here.
#
# `-e VAR` (no value) passes the host env var through only when set,
# otherwise leaves it unset in the container — so build.sh's own
# defaults kick in. Setting `-e VAR=...` here would shadow them.
docker run --rm --privileged \
    -e VERSION \
    -e ARCH \
    -e DEBIAN_VERSION \
    -e DEBIAN_RELEASE \
    -e WORK_DIR=/work/build \
    -e OUTPUT_DIR=/work/output \
    -v "$SCRIPT_DIR":/work \
    "$IMAGE_TAG"

echo
echo "Done. Artefacts in $SCRIPT_DIR/output:"
ls -lh "$SCRIPT_DIR/output"
echo
echo "Next step:"
echo "  forge image import $SCRIPT_DIR/output/forge-base-${VERSION:-dev}-${ARCH:-arm64}.img.gz"
