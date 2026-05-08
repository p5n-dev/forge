#!/usr/bin/env bash
# Refresh pinned dependency versions + integrity hashes across FORGE.
#
# Supply-chain rules (HARD requirements — see CLAUDE.md):
#   1. Every external dependency we add MUST be pinned by version AND
#      content hash (digest / SHA256). Version-only pins are NOT enough
#      because tags are mutable on most registries.
#   2. Only pin releases that have been published for at least
#      SOAK_DAYS (default 14) — gives the wider community time to
#      surface bugs and CVEs before FORGE adopts them.
#
# What gets pinned:
#   - Forgejo runtime image (codeberg.org/forgejo/forgejo) — version + sha256 digest
#       Source of truth: Codeberg release API; digest from local docker pull.
#   - Builder base image (debian:13-slim) — sha256 digest
#       Source of truth: Docker Hub. Debian's slim tag is rebuilt
#       continuously with security patches, so we pin whatever
#       digest is current at refresh time.
#   - k3s installer script (get.k3s.io) — sha256 only (no version,
#       it's "latest" and always served from the same URL)
#       Source of truth: live curl of get.k3s.io.
#   - claude-code installer script (claude.ai/install.sh) — sha256 only
#       Source of truth: live curl of claude.ai/install.sh. The
#       claude-code release version itself is configurable per-env via
#       forge.yaml (passed as TARGET to install.sh) — only the script
#       blob we exec needs a content pin.
#   - helm installer script (helm/helm get-helm-3) — sha256 only
#       Source of truth: live curl of
#       raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3.
#       Same model as the claude-code installer: helm release version
#       is configurable per-env via forge.yaml (passed as
#       DESIRED_VERSION env var) — only the wrapper needs a content pin.
#   - Debian cloud genericcloud-arm64 .qcow2 — build-id + sha512
#       Source of truth: cloud.debian.org dated build dirs.
#       Picks the latest dated build passing the soak rule, fetches
#       SHA512SUMS, extracts the genericcloud-arm64 line.
#   - syft installer + version — install.sh content sha256 + release tag
#       Source of truth: anchore/syft GitHub releases + raw install.sh.
#       Both pinned because we exec install.sh AND tell it which
#       release to fetch — independent attack surfaces.
#
# How it edits source files:
#   - internal/forgejo/pins.go: sed-replaces defaultImageTag and
#     defaultImageDigest constants in place.
#   - images/base/Dockerfile: rewrites the FROM debian:13-slim@... line
#     and the SYFT_VERSION / SYFT_INSTALLER_SHA256 ARGs.
#   - images/base/build.sh: rewrites DEBIAN_CLOUD_BUILD_ID and
#     DEBIAN_CLOUD_QCOW2_SHA512.
#   - .github/workflows/image.yml: rewrites SYFT_VERSION + SYFT_INSTALLER_SHA256
#     env: keys (kept in lock-step with the Dockerfile).
#   - internal/cloudinit/userdata.go: rewrites k3sInstallerSHA256,
#     claudeCodeInstallerSHA256, and helmInstallerSHA256.
#
# Run this from anywhere in the repo. After it succeeds, review the
# diff (it prints one) and commit. Set GITHUB_TOKEN to avoid the 60/hr
# unauthenticated GitHub API rate limit.

set -euo pipefail

SOAK_DAYS="${SOAK_DAYS:-14}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
PIN_FILE="$REPO_ROOT/internal/forgejo/pins.go"
DOCKERFILE="$REPO_ROOT/images/base/Dockerfile"
USERDATA_FILE="$REPO_ROOT/internal/cloudinit/userdata.go"
BUILD_SH="$REPO_ROOT/images/base/build.sh"
IMAGE_WORKFLOW="$REPO_ROOT/.github/workflows/image.yml"

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "missing required tool: $1" >&2
        exit 1
    fi
}
require curl
require jq
require docker
require sed
require git
require sha256sum

# gh_curl wraps curl with optional GitHub auth — significantly raises
# the unauthenticated 60/hr rate limit when GITHUB_TOKEN is set.
gh_curl() {
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
        curl -fsSL --max-time 30 -H "Authorization: Bearer ${GITHUB_TOKEN}" "$@"
    else
        curl -fsSL --max-time 30 "$@"
    fi
}

# iso_to_unix accepts an ISO-8601 timestamp and prints unix seconds.
# Tries GNU date first (Linux) then BSD date (macOS).
iso_to_unix() {
    local ts="$1"
    # Codeberg returns timestamps like "2026-04-29T15:09:57+02:00", which
    # BSD date can't parse with a single fixed format. Strip fractional
    # seconds and any TZ suffix (Z or ±HH:MM); the soak-day check is at
    # day granularity, so dropping the offset is harmless.
    ts="${ts%%.*}"
    ts="${ts%Z}"
    ts="${ts%[+-][0-9][0-9]:[0-9][0-9]}"
    date -u -d "${ts}Z" +%s 2>/dev/null \
        || date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "${ts}Z" +%s
}

now_unix="$(date -u +%s)"
cutoff_unix=$((now_unix - SOAK_DAYS * 86400))

# ----------------------------------------------------------------------
# Forgejo
# ----------------------------------------------------------------------

echo "==> Forgejo"
echo "    looking for latest stable release older than ${SOAK_DAYS} days"

# Fetch a generous slice of releases so we can scan past any newer
# ones that haven't met the soak rule yet.
releases_json="$(curl -fsSL \
    "https://codeberg.org/api/v1/repos/forgejo/forgejo/releases?limit=30")"

forgejo_tag=""
while IFS=$'\t' read -r tag published; do
    pub_unix="$(iso_to_unix "$published")"
    if [[ "$pub_unix" -le "$cutoff_unix" ]]; then
        forgejo_tag="$tag"
        forgejo_pub="$published"
        break
    fi
done < <(jq -r '
    map(select(.draft == false and .prerelease == false))
    | sort_by(.published_at) | reverse
    | .[] | [.tag_name, .published_at] | @tsv' <<<"$releases_json")

if [[ -z "$forgejo_tag" ]]; then
    echo "    no Forgejo release older than ${SOAK_DAYS} days found — aborting" >&2
    exit 1
fi

# Forgejo's container image tags drop the leading 'v'.
forgejo_version="${forgejo_tag#v}"
forgejo_ref="codeberg.org/forgejo/forgejo:${forgejo_version}"

echo "    candidate: ${forgejo_tag} (published ${forgejo_pub})"
echo "    pulling   ${forgejo_ref}"
docker pull --quiet "${forgejo_ref}" >/dev/null

forgejo_digest="$(docker inspect --format='{{index .RepoDigests 0}}' \
    "${forgejo_ref}" | sed 's|.*@||')"
if [[ -z "$forgejo_digest" || "$forgejo_digest" != sha256:* ]]; then
    echo "    could not resolve digest for ${forgejo_ref}" >&2
    exit 1
fi
echo "    digest    ${forgejo_digest}"

# ----------------------------------------------------------------------
# Debian builder
# ----------------------------------------------------------------------

echo "==> Debian builder (debian:13-slim)"
echo "    pulling debian:13-slim"
docker pull --quiet debian:13-slim >/dev/null

debian_digest="$(docker inspect --format='{{index .RepoDigests 0}}' \
    debian:13-slim | sed 's|.*@||')"
if [[ -z "$debian_digest" || "$debian_digest" != sha256:* ]]; then
    echo "    could not resolve digest for debian:13-slim" >&2
    exit 1
fi
echo "    digest    ${debian_digest}"

# ----------------------------------------------------------------------
# k3s installer script
# ----------------------------------------------------------------------

echo "==> k3s installer (get.k3s.io)"
echo "    fetching the live installer and hashing it"

k3s_installer_tmp="$(mktemp)"
trap 'rm -f "$k3s_installer_tmp"' EXIT
curl -fsSL --max-time 60 https://get.k3s.io -o "$k3s_installer_tmp"
k3s_installer_sha256="$(sha256sum "$k3s_installer_tmp" | awk '{print $1}')"
if [[ -z "$k3s_installer_sha256" ]]; then
    echo "    could not hash k3s installer" >&2
    exit 1
fi
echo "    sha256    ${k3s_installer_sha256}"

# ----------------------------------------------------------------------
# helm installer script (helm/helm/scripts/get-helm-3)
# ----------------------------------------------------------------------

echo "==> helm installer (get-helm-3)"
echo "    fetching the live installer and hashing it"

helm_installer_tmp="$(mktemp)"
trap 'rm -f "$k3s_installer_tmp" "$helm_installer_tmp"' EXIT
curl -fsSL --max-time 60 \
    https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
    -o "$helm_installer_tmp"
helm_installer_sha256="$(sha256sum "$helm_installer_tmp" | awk '{print $1}')"
if [[ -z "$helm_installer_sha256" ]]; then
    echo "    could not hash helm installer" >&2
    exit 1
fi
echo "    sha256    ${helm_installer_sha256}"

# ----------------------------------------------------------------------
# claude-code installer script (claude.ai/install.sh)
# ----------------------------------------------------------------------
#
# Anthropic's official installer for claude-code. We pin the script's
# content sha256; the version of claude-code that ends up installed is
# chosen at env-create time via forge.yaml's bootstrap.claude_code (a
# `stable`, `latest`, or bare-semver TARGET passed to the script).
# install.sh's own manifest verification covers the binary; our pin
# covers the wrapper.

echo "==> claude-code installer (claude.ai/install.sh)"
echo "    fetching the live installer and hashing it"

claude_installer_tmp="$(mktemp)"
trap 'rm -f "$k3s_installer_tmp" "$helm_installer_tmp" "$claude_installer_tmp"' EXIT
curl -fsSL --max-time 60 https://claude.ai/install.sh -o "$claude_installer_tmp"
claude_installer_sha256="$(sha256sum "$claude_installer_tmp" | awk '{print $1}')"
if [[ -z "$claude_installer_sha256" ]]; then
    echo "    could not hash claude-code installer" >&2
    exit 1
fi
echo "    sha256    ${claude_installer_sha256}"

# ----------------------------------------------------------------------
# Debian cloud image (trixie, genericcloud-arm64)
# ----------------------------------------------------------------------

echo "==> Debian cloud image (trixie, genericcloud-arm64)"
echo "    looking for latest dated build older than ${SOAK_DAYS} days"

# cloud.debian.org publishes dated build directories named YYYYMMDD-NNNN.
# We list, filter to ones whose date prefix is past the soak cutoff,
# and pick the most recent.
debian_index="$(curl -fsSL --max-time 30 \
    "https://cloud.debian.org/images/cloud/trixie/")"
debian_build_id=""
while read -r build; do
    # build like "20260413-2447" — first 8 chars are YYYYMMDD.
    build_date="${build:0:8}"
    # Convert YYYYMMDD to ISO date for iso_to_unix.
    iso="${build_date:0:4}-${build_date:4:2}-${build_date:6:2}T00:00:00Z"
    pub_unix="$(iso_to_unix "$iso")"
    if [[ "$pub_unix" -le "$cutoff_unix" ]]; then
        debian_build_id="$build"
        debian_build_date="$iso"
        break
    fi
done < <(printf '%s\n' "$debian_index" \
    | grep -oE 'href="20[0-9]+-[0-9]+/"' \
    | sed 's|href="||;s|/"$||' \
    | sort -u | tac)

if [[ -z "$debian_build_id" ]]; then
    echo "    no Debian cloud build older than ${SOAK_DAYS} days found — aborting" >&2
    exit 1
fi

echo "    candidate: ${debian_build_id} (date ${debian_build_date%T*})"

debian_qcow2_sha512="$(curl -fsSL --max-time 30 \
    "https://cloud.debian.org/images/cloud/trixie/${debian_build_id}/SHA512SUMS" \
    | awk -v target="debian-13-genericcloud-arm64-${debian_build_id}.qcow2" \
        '$2 == target {print $1}')"
if [[ -z "$debian_qcow2_sha512" ]]; then
    echo "    could not extract SHA512 for genericcloud-arm64.qcow2 in build ${debian_build_id}" >&2
    exit 1
fi
echo "    sha512    ${debian_qcow2_sha512}"

# ----------------------------------------------------------------------
# syft installer + version
# ----------------------------------------------------------------------
#
# We pin BOTH:
#   - the syft version we install (passed to install.sh -v)
#   - the install.sh script's own SHA256 (the bytes we exec)
# That way an attacker would need to compromise both the GitHub
# release artifact AND the install.sh on raw.githubusercontent.com
# in a way that produces our exact pinned content.

echo "==> syft installer"
echo "    looking for latest stable release older than ${SOAK_DAYS} days"

syft_releases_json="$(gh_curl \
    "https://api.github.com/repos/anchore/syft/releases?per_page=30")"

syft_version=""
while IFS=$'\t' read -r tag published; do
    pub_unix="$(iso_to_unix "$published")"
    if [[ "$pub_unix" -le "$cutoff_unix" ]]; then
        syft_version="${tag#v}"
        syft_pub="$published"
        break
    fi
done < <(jq -r '
    map(select(.draft == false and .prerelease == false))
    | sort_by(.published_at) | reverse
    | .[] | [.tag_name, .published_at] | @tsv' <<<"$syft_releases_json")

if [[ -z "$syft_version" ]]; then
    echo "    no syft release older than ${SOAK_DAYS} days found — aborting" >&2
    exit 1
fi

echo "    candidate: v${syft_version} (published ${syft_pub})"

# Hash install.sh from anchore/syft@main. The script itself is what we
# pipe into sh, so its content has to be pinned independently of the
# version we tell it to install.
syft_install_tmp="$(mktemp)"
trap 'rm -f "$k3s_installer_tmp" "$helm_installer_tmp" "$claude_installer_tmp" "$syft_install_tmp"' EXIT
curl -fsSL --max-time 30 \
    https://raw.githubusercontent.com/anchore/syft/main/install.sh \
    -o "$syft_install_tmp"
syft_installer_sha256="$(sha256sum "$syft_install_tmp" | awk '{print $1}')"
if [[ -z "$syft_installer_sha256" ]]; then
    echo "    could not hash syft install.sh" >&2
    exit 1
fi
echo "    install.sh sha256 ${syft_installer_sha256}"

# ----------------------------------------------------------------------
# Apply
# ----------------------------------------------------------------------

# In-place sed with portable backup-then-delete pattern (works on both
# GNU and BSD sed without the -i quirks).
inplace_sed() {
    local file="$1" pattern="$2"
    sed "$pattern" "$file" > "$file.tmp"
    mv "$file.tmp" "$file"
}

echo "==> Updating ${PIN_FILE#$REPO_ROOT/}"
# Use POSIX [[:space:]] — BSD sed treats \s as a literal 's' and would
# silently skip the substitution, leaving placeholder pins in place.
inplace_sed "$PIN_FILE" \
    "s|defaultImageTag[[:space:]]*=.*pin:forgejo-tag|defaultImageTag    = \"${forgejo_version}\" // pin:forgejo-tag|"
inplace_sed "$PIN_FILE" \
    "s|defaultImageDigest[[:space:]]*=.*pin:forgejo-digest|defaultImageDigest = \"${forgejo_digest}\" // pin:forgejo-digest|"

echo "==> Updating ${DOCKERFILE#$REPO_ROOT/}"
inplace_sed "$DOCKERFILE" \
    "s|^FROM debian:13-slim@sha256:[a-f0-9]*|FROM debian:13-slim@${debian_digest}|"

echo "==> Updating ${USERDATA_FILE#$REPO_ROOT/}"
inplace_sed "$USERDATA_FILE" \
    "s|k3sInstallerSHA256[[:space:]]*=.*pin:k3s-installer-sha256|k3sInstallerSHA256 = \"${k3s_installer_sha256}\" // pin:k3s-installer-sha256|"
inplace_sed "$USERDATA_FILE" \
    "s|claudeCodeInstallerSHA256[[:space:]]*=.*pin:claude-installer-sha256|claudeCodeInstallerSHA256 = \"${claude_installer_sha256}\" // pin:claude-installer-sha256|"
inplace_sed "$USERDATA_FILE" \
    "s|helmInstallerSHA256[[:space:]]*=.*pin:helm-installer-sha256|helmInstallerSHA256 = \"${helm_installer_sha256}\" // pin:helm-installer-sha256|"

echo "==> Updating ${BUILD_SH#$REPO_ROOT/}"
inplace_sed "$BUILD_SH" \
    "s|DEBIAN_CLOUD_BUILD_ID=.*pin:debian-cloud-build-id|DEBIAN_CLOUD_BUILD_ID=\"${debian_build_id}\" # pin:debian-cloud-build-id|"
inplace_sed "$BUILD_SH" \
    "s|DEBIAN_CLOUD_QCOW2_SHA512=.*pin:debian-cloud-qcow2-sha512|DEBIAN_CLOUD_QCOW2_SHA512=\"${debian_qcow2_sha512}\" # pin:debian-cloud-qcow2-sha512|"

echo "==> Updating ${DOCKERFILE#$REPO_ROOT/} (syft pins)"
inplace_sed "$DOCKERFILE" \
    "s|^ARG SYFT_VERSION=.*|ARG SYFT_VERSION=${syft_version}|"
inplace_sed "$DOCKERFILE" \
    "s|^ARG SYFT_INSTALLER_SHA256=.*|ARG SYFT_INSTALLER_SHA256=${syft_installer_sha256}|"

echo "==> Updating ${IMAGE_WORKFLOW#$REPO_ROOT/} (syft pins)"
# YAML 'env:' lines have leading whitespace. Anchor on the key.
inplace_sed "$IMAGE_WORKFLOW" \
    "s|SYFT_VERSION:.*|SYFT_VERSION: ${syft_version}|"
inplace_sed "$IMAGE_WORKFLOW" \
    "s|SYFT_INSTALLER_SHA256:.*|SYFT_INSTALLER_SHA256: ${syft_installer_sha256}|"

# Re-format Go files in case sed produced weird whitespace.
if command -v gofmt >/dev/null 2>&1; then
    gofmt -w "$PIN_FILE" "$USERDATA_FILE"
fi

echo
echo "Pins refreshed:"
echo "  forgejo:         ${forgejo_version} @ ${forgejo_digest}"
echo "  debian-builder:  13-slim   @ ${debian_digest}"
echo "  k3s installer:   sha256:${k3s_installer_sha256}"
echo "  helm installer:  sha256:${helm_installer_sha256}"
echo "  claude install:  sha256:${claude_installer_sha256}"
echo "  debian cloud:    build ${debian_build_id} @ sha512:${debian_qcow2_sha512}"
echo "  syft:            v${syft_version} (install.sh @ sha256:${syft_installer_sha256})"
echo
echo "Diff:"
git --no-pager diff -- "$PIN_FILE" "$DOCKERFILE" "$USERDATA_FILE" "$BUILD_SH" "$IMAGE_WORKFLOW" || true
