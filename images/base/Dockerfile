# Build the FORGE thin base image inside a Linux container, so macOS
# developers (and anyone without libguestfs on their host) can iterate
# locally without setting up a Linux VM.
#
# Usage is wrapped by ./build-in-docker.sh — that script handles volume
# mounts, env-var passthrough, and the `--privileged` flag libguestfs
# requires to spawn its appliance VM.

# pin:debian-builder — refresh with ./scripts/refresh-pins.sh.
# Debian's slim tag gets rebuilt with security patches, so this
# digest drifts whenever upstream releases a new build. Re-run the
# refresh script periodically, or after any relevant CVE.
FROM debian:13-slim@sha256:cedb1ef40439206b673ee8b33a46a03a0c9fa90bf3732f54704f99cb061d2c5a

ENV DEBIAN_FRONTEND=noninteractive

# apt-installed packages are intentionally NOT pinned to specific
# versions. Trust chain: the base image above is digest-pinned, so
# the apt sources list + trusted keys are frozen; apt then verifies
# each .deb against Debian's GPG-signed Release file. See CLAUDE.md
# § "Supply-chain hardening" for the full rationale.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gzip \
        libguestfs-tools \
        linux-image-generic \
        qemu-utils \
        qemu-system-arm \
        qemu-system-x86 \
        qemu-user-static \
 && rm -rf /var/lib/apt/lists/*

# syft for SBOM generation. Both pins refreshed by scripts/refresh-pins.sh
# under the 14-day soak rule:
#   - SYFT_VERSION         the syft release we install (passed as a
#                          POSITIONAL argument at the end of install.sh's
#                          argv — NOT as -v: that flag enables cosign
#                          signature verification in current versions
#                          of install.sh, which we don't have cosign
#                          installed for)
#   - SYFT_INSTALLER_SHA256 the content hash of install.sh itself, since
#                           that's the script we're piping into sh
ARG SYFT_VERSION=1.43.0
ARG SYFT_INSTALLER_SHA256=8a277b4b4dc21809ecd9a0e4926f4d7319affc6451a607fdeb3e5e1fbd9bc573
RUN curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh -o /tmp/syft-install.sh \
 && echo "${SYFT_INSTALLER_SHA256}  /tmp/syft-install.sh" | sha256sum -c - \
 && sh /tmp/syft-install.sh -b /usr/local/bin "v${SYFT_VERSION}" \
 && rm -f /tmp/syft-install.sh

# libguestfs needs to read the host kernel image; on a slim base image the
# kernel lives under /boot. We installed linux-image-generic above so this
# path exists; mark it world-readable so libguestfs doesn't fail with
# "permission denied reading /boot/vmlinuz-*".
RUN chmod 0644 /boot/vmlinuz-* || true

WORKDIR /work
ENTRYPOINT ["/work/build.sh"]
