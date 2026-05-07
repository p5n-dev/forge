#!/bin/bash
# Publish the FORGE base image to a GitHub Release.
#
# Required:
#   gh CLI authenticated (GH_TOKEN env or `gh auth login`)
#
# Environment variables:
#   VERSION    — image version tag (required)
#   ARCH       — target architecture (default: arm64)
#   OUTPUT_DIR — where build.sh wrote artefacts (default: ./output)
#   REPO       — GitHub repo, owner/name (default: schubergphilis/forge)

set -euo pipefail

VERSION="${VERSION:?VERSION must be set}"
ARCH="${ARCH:-arm64}"
OUTPUT_DIR="${OUTPUT_DIR:-$(pwd)/output}"
REPO="${REPO:-schubergphilis/forge}"

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "error: required tool not found: $1" >&2
        exit 1
    }
}

require gh

IMG="$OUTPUT_DIR/forge-base-${VERSION}-${ARCH}.img.gz"
SBOM_SPDX="$OUTPUT_DIR/sbom.spdx.json"
SBOM_CDX="$OUTPUT_DIR/sbom.cdx.json"
SUMS="$OUTPUT_DIR/SHA256SUMS"

for f in "$IMG" "$SBOM_SPDX" "$SBOM_CDX" "$SUMS"; do
    [ -f "$f" ] || { echo "error: missing artefact: $f" >&2; exit 1; }
done

TAG="v${VERSION}"

echo "==> Publishing release ${TAG} to ${REPO}"
gh release create "$TAG" \
    --repo "$REPO" \
    --title "FORGE base image ${TAG}" \
    --notes "Thin Debian 12 ${ARCH} base image for FORGE. See attached SBOMs for full contents." \
    "$IMG" \
    "$SBOM_SPDX" \
    "$SBOM_CDX" \
    "$SUMS"

echo "Done."
