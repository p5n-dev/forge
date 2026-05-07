#!/bin/bash
# Build the FORGE thin base VM image.
#
# Produces:
#   ${OUTPUT_DIR}/forge-base-${VERSION}-arm64.img.gz
#   ${OUTPUT_DIR}/sbom.spdx.json
#   ${OUTPUT_DIR}/sbom.cdx.json
#   ${OUTPUT_DIR}/SHA256SUMS
#
# Required tools (install on the build host):
#   - libguestfs-tools (provides virt-customize)
#   - qemu-utils       (provides qemu-img)
#   - syft             (Anchore SBOM generator)
#   - curl, gzip, sha256sum, awk
#
# Environment variables (with defaults):
#   VERSION         — image version tag (default: dev)
#   ARCH            — target architecture (default: arm64)
#   DEBIAN_VERSION  — Debian major version, e.g. 12, 13 (default: 13)
#   DEBIAN_RELEASE  — release codename. Looked up from upstream
#                     distro-info-data when unset; set explicitly only
#                     if you want to override (e.g. point at unstable/sid).
#   WORK_DIR        — scratch space for downloads / intermediate (default: ./build)
#   OUTPUT_DIR      — where final artefacts land (default: ./output)

set -euo pipefail

# Supply-chain pins for the Debian cloud base image. Refreshed by
# scripts/refresh-pins.sh under the 14-day soak rule — do NOT edit
# by hand. The build ID identifies a specific dated cloud-image
# build (cloud.debian.org/images/cloud/<release>/<build-id>/), and
# the SHA512 is the hash of the genericcloud-arm64 .qcow2 inside it.
#
# These pins are tied to (DEBIAN_VERSION=13, ARCH=arm64). If a caller
# overrides either, the verify step is skipped with a loud warning —
# we can't pre-compute hashes for every (release × arch) combination,
# and silently shipping an unverified image would be worse than
# refusing.
DEBIAN_CLOUD_BUILD_ID="20260413-2447" # pin:debian-cloud-build-id
DEBIAN_CLOUD_QCOW2_SHA512="9de87e5e9739ec07fe61d40e5a6796e6e97e557ddb7d1eee5cf28619e80c21c0da76fc54713161e52c2719a38b5376e7b3bcfcd468e0b82ab9d33f702a09d601" # pin:debian-cloud-qcow2-sha512

VERSION="${VERSION:-dev}"
ARCH="${ARCH:-arm64}"
DEBIAN_VERSION="${DEBIAN_VERSION:-13}"
WORK_DIR="${WORK_DIR:-$(pwd)/build}"
OUTPUT_DIR="${OUTPUT_DIR:-$(pwd)/output}"

mkdir -p "$WORK_DIR"

# Resolve DEBIAN_RELEASE (codename) from DEBIAN_VERSION using Debian's
# canonical version⇄codename mapping at distro-info-data. Cached locally
# so subsequent runs are offline-friendly. Set DEBIAN_RELEASE explicitly
# to skip the lookup entirely.
if [ -z "${DEBIAN_RELEASE:-}" ]; then
    DEBIAN_INFO_URL="https://salsa.debian.org/debian/distro-info-data/-/raw/main/debian.csv"
    DEBIAN_INFO_CSV="$WORK_DIR/debian-distro-info.csv"

    if [ ! -s "$DEBIAN_INFO_CSV" ]; then
        echo "==> Fetching Debian distro info ($DEBIAN_INFO_URL)"
        curl -fsSL --retry 3 -o "$DEBIAN_INFO_CSV" "$DEBIAN_INFO_URL"
    fi

    # CSV columns: version,codename,series,created,release,eol,...
    # We want the lowercase `series` (column 3) for cloud-image URLs.
    DEBIAN_RELEASE=$(awk -F, -v v="$DEBIAN_VERSION" \
        'NR>1 && $1 == v { print $3; exit }' "$DEBIAN_INFO_CSV")

    if [ -z "$DEBIAN_RELEASE" ]; then
        echo "error: Debian version $DEBIAN_VERSION not found in $DEBIAN_INFO_URL" >&2
        echo "       set DEBIAN_RELEASE explicitly to override" >&2
        exit 1
    fi
fi

# Use the dated build-ID directory, not /latest/, so the URL points at
# a specific image bytes-for-bytes. The filename inside the dated dir
# embeds the build ID (different from the un-dated name in /latest/).
BASE_IMAGE_FILENAME="debian-${DEBIAN_VERSION}-genericcloud-${ARCH}-${DEBIAN_CLOUD_BUILD_ID}.qcow2"
BASE_IMAGE_URL="https://cloud.debian.org/images/cloud/${DEBIAN_RELEASE}/${DEBIAN_CLOUD_BUILD_ID}/${BASE_IMAGE_FILENAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# build.sh shells out to libguestfs (virt-customize) which is Linux-only.
# Catch the macOS case up-front so users get the right answer instead of
# hitting "command not found" later.
if [[ "$(uname -s)" == "Darwin" ]]; then
    cat >&2 <<'EOF'
error: build.sh uses libguestfs (virt-customize), which is Linux-only.

       On macOS, run the Docker wrapper instead — it builds the image
       inside a Linux container so no host-side toolchain is required:

           ./build-in-docker.sh

       Same arguments, same output, identical artefacts.
EOF
    exit 1
fi

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "error: required tool not found: $1" >&2
        exit 1
    }
}

require virt-customize
require qemu-img
require syft
require curl
require gzip
require sha256sum
require sha512sum
require awk

mkdir -p "$WORK_DIR" "$OUTPUT_DIR"

# Cache filename includes the build ID so a pin change automatically
# triggers a fresh download instead of silently reusing a stale qcow2.
CACHE_IMG="$WORK_DIR/$BASE_IMAGE_FILENAME"
WORK_IMG="$WORK_DIR/forge-base-${VERSION}-${ARCH}.qcow2"
RAW_IMG="$WORK_DIR/forge-base-${VERSION}-${ARCH}.img"
OUT_IMG="$OUTPUT_DIR/forge-base-${VERSION}-${ARCH}.img.gz"

echo "==> Downloading Debian ${DEBIAN_VERSION} ${ARCH} cloud image (cached if present)"
echo "    build-id ${DEBIAN_CLOUD_BUILD_ID}"
if [ ! -f "$CACHE_IMG" ]; then
    curl -fL --retry 3 -o "$CACHE_IMG" "$BASE_IMAGE_URL"
fi

# Verify the qcow2 against the pinned SHA512. The pin is tied to
# (DEBIAN_VERSION=13, ARCH=arm64); other combinations would need their
# own pins, which we don't currently track.
if [ "$DEBIAN_VERSION" = "13" ] && [ "$ARCH" = "arm64" ]; then
    echo "==> Verifying qcow2 against pinned SHA512"
    echo "${DEBIAN_CLOUD_QCOW2_SHA512}  $CACHE_IMG" | sha512sum -c -
else
    cat >&2 <<EOF
warning: skipping qcow2 SHA512 verification — DEBIAN_VERSION=${DEBIAN_VERSION},
         ARCH=${ARCH} differs from the pinned (13, arm64). FORGE only
         tracks SHA512 for the default combination today; off-default
         builds run at your own supply-chain risk.
EOF
fi

echo "==> Copying to working image"
cp "$CACHE_IMG" "$WORK_IMG"

echo "==> Customizing image (installing packages, registering forge-ready service)"
virt-customize -a "$WORK_IMG" \
    --update \
    --install openssh-server,cloud-init,curl,git,ca-certificates,sudo,socat,jq \
    --copy-in "$SCRIPT_DIR/files/forge-ready.service:/etc/systemd/system/" \
    --copy-in "$SCRIPT_DIR/files/forge-ready.sh:/usr/local/bin/" \
    --copy-in "$SCRIPT_DIR/files/forge-vsock.conf:/etc/modules-load.d/" \
    --run-command "mv /usr/local/bin/forge-ready.sh /usr/local/bin/forge-ready" \
    --run-command "chmod +x /usr/local/bin/forge-ready" \
    --run-command "systemctl enable forge-ready.service" \
    --run-command "apt-get clean" \
    --truncate /etc/machine-id

echo "==> Converting to raw"
qemu-img convert -f qcow2 -O raw "$WORK_IMG" "$RAW_IMG"

echo "==> Compressing"
gzip -9 -c "$RAW_IMG" > "$OUT_IMG"

echo "==> Generating SBOMs (SPDX + CycloneDX)"
syft -q "$WORK_IMG" -o spdx-json="$OUTPUT_DIR/sbom.spdx.json"
syft -q "$WORK_IMG" -o cyclonedx-json="$OUTPUT_DIR/sbom.cdx.json"

echo "==> Generating SHA256SUMS"
(
    cd "$OUTPUT_DIR"
    sha256sum \
        "forge-base-${VERSION}-${ARCH}.img.gz" \
        "sbom.spdx.json" \
        "sbom.cdx.json" \
        > SHA256SUMS
)

echo
echo "Done. Artefacts:"
ls -lh "$OUTPUT_DIR"
