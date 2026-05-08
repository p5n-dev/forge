#!/bin/bash
# Sends the FORGE boot-complete signal over virtio-vsock to the host.
# Runs once on first boot after cloud-init finishes (see forge-ready.service).
set -euo pipefail

IP=$(hostname -I | awk '{print $1}')
echo "ready addr=${IP}" | socat - VSOCK-CONNECT:2:1234
