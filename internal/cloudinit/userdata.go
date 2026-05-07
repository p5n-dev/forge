// Package cloudinit renders cloud-init user-data, meta-data, and network-config,
// and packages them into a NoCloud-compatible ISO9660 image (volume label
// "cidata").
package cloudinit

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/url"
	"text/template"
)

// statuslineSh is the bash script that renders the Claude Code status
// line inside an env's VM. Embedded so a single forge binary carries
// it — no extra files to ship, no host-path lookup at render time.
// Cloud-init writes it to /home/forge/.claude/statusline.sh and the
// minimal settings.json next to it points at it.
//
//go:embed files/statusline.sh
var statuslineSh []byte

// UserDataInput is everything FORGE templates into the cloud-init user-data
// at `forge env create` time.
type UserDataInput struct {
	// Name is the env name (without the "forge-" hostname prefix).
	Name string
	// AuthorizedKey is a single line in authorized_keys format
	// ("ssh-ed25519 AAAA... comment").
	AuthorizedKey string
	// K3sVersion, RageVersion, ClaudeCodeVersion, HelmVersion are the
	// pinned versions from forge.yaml.
	K3sVersion        string
	RageVersion       string
	ClaudeCodeVersion string
	HelmVersion       string
	// ForgejoRemoteURL is optional. When set, it is recorded in
	// /etc/forge/forgejo-remote so the agent can `git remote add` it.
	ForgejoRemoteURL string
	// HostUID, when > 0, is set as the forge user's UID inside the VM.
	// We point it at the host user's UID so files in the virtio-fs
	// workspace share line up with the same numeric owner on both
	// sides. 0 → cloud-init picks a default (typically 1000).
	HostUID int
	// ForgejoHostBase is the URL FORGE uses for the Forgejo instance,
	// e.g. http://localhost:4000. The same URL is reachable from inside
	// the VM because the in-VM forge-forgejo-vsock.service forwards
	// 127.0.0.1:<port> over vsock to the host-side proxy (see
	// internal/forgejoproxy). No host/VM URL split — they are the same
	// string in both views.
	ForgejoHostBase string
	// ForgejoVsockPort is the port the in-VM socat unit listens on
	// (TCP, on 127.0.0.1) and connects to over vsock. Conventionally
	// the same port as Forgejo on the host so URLs match across the
	// boundary.
	ForgejoVsockPort int
	// ForgejoUser / ForgejoToken populate the forge user's
	// ~/.git-credentials so `git push` from inside the VM works
	// without prompting. Empty → no credential file is written.
	ForgejoUser  string
	ForgejoToken string
	// GitUserName / GitUserEmail are baked into /home/forge/.gitconfig
	// so the in-VM agent can `git commit` without prompting for an
	// identity. Required: a missing identity blocks every first
	// commit. The caller chooses the values (typically the env name
	// and "<name>@forge.local" so commits attribute to the per-env
	// Forgejo user).
	GitUserName  string
	GitUserEmail string
}

// In-VM bootstrap pins. Each runtime fetch from outside the base
// image is pinned by version AND sha256 (per the supply-chain rule in
// CLAUDE.md). All constants below are rewritten by
// `scripts/refresh-pins.sh` using the `pin:<name>` sentinel comments;
// do NOT edit by hand — the script enforces the 14-day soak rule and
// re-derives hashes from the actual artifact.
const (
	// k3s installer script. The k3s binary itself is verified by
	// this script's own SHA256 check (it pulls SHA256SUMS from the
	// k3s release), but the script blob is what we curl-pipe-bash —
	// pin its content too so a swapped installer can't change what
	// gets executed. INSTALL_K3S_VERSION (from forge.yaml) is
	// passed via env var, so the k3s version itself is independent
	// of this pin.
	k3sInstallerSHA256 = "46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad" // pin:k3s-installer-sha256

	// helm installer script (helm.sh's get-helm-3 from the helm/helm
	// repo). Like the k3s installer, the helm script downloads the
	// helm binary from helm's release server and verifies it against
	// helm's own SHA256SUMS — but the script blob is what we exec, so
	// pin its content. The helm version itself is configurable via
	// forge.yaml's bootstrap.helm (passed as DESIRED_VERSION env var).
	helmInstallerSHA256 = "38b65f882d9cae3891755bdb03becc6a01ae6f9cb24826c191f219ddfee70a5d" // pin:helm-installer-sha256

	// claude-code installer script (https://claude.ai/install.sh).
	// This is the official install path documented at
	// code.claude.com/docs — a self-contained shell script that
	// downloads a native single-file binary, verifies it against
	// claude.ai's manifest.json, then runs `claude install <target>`
	// to set up the persistent launcher in $HOME/.local/bin and
	// shell integration.
	//
	// We pin the SCRIPT content's sha256 because that's the bytes
	// we exec. The version of claude-code that ends up installed is
	// chosen by ClaudeCodeVersion (forge.yaml → bootstrap.claude_code,
	// passed as the script's TARGET arg). install.sh accepts
	// `stable`, `latest`, or a bare semver — we strip a leading `v`
	// in the bootstrap so v-prefixed pins still work.
	//
	// Trust chain: install.sh's own verification matches the
	// bootstrap-time download against claude.ai's signed manifest.
	// Our pin on install.sh's content protects the wrapper itself
	// from silent tampering.
	claudeCodeInstallerSHA256 = "b315b46925a9bfb9422f2503dd5aa649f680832f4c076b22d87c39d578c3d830" // pin:claude-installer-sha256
)

const userDataTemplate = `#cloud-config
hostname: forge-{{.Name}}
manage_etc_hosts: true

users:
  - name: forge
{{- if gt .HostUID 0}}
    uid: {{.HostUID}}
{{- end}}
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    groups: sudo
    lock_passwd: false
    plain_text_passwd: debug
    ssh_authorized_keys:
      - {{.AuthorizedKey}}

chpasswd:
  expire: false

# Mount the env's host-shared workspace at /home/forge/workspace via
# virtio-fs. cloud-init writes this to /etc/fstab so it auto-mounts on
# every boot. nofail keeps boot succeeding on older envs whose vfkit
# args predate the workspace-share device.
mounts:
  - [ workspace-share, /home/forge/workspace, virtiofs, "defaults,nofail", "0", "0" ]

write_files:
  - path: /etc/modules-load.d/forge-virtiofs.conf
    content: |
      virtiofs

  - path: /etc/forge/forgejo-remote
    content: |
      {{.ForgejoRemoteURL}}
    permissions: '0644'

{{- if .GitCredentialURL}}

  # forge user's git credential store. The URL written here matches
  # the URL the host clone recorded in .git/config because the in-VM
  # forge-forgejo-vsock.service makes localhost:<port> reachable from
  # inside the VM (it forwards over vsock to the host-side proxy).
  # defer:true ensures cloud-init writes this AFTER the user exists.
  - path: /home/forge/.git-credentials
    permissions: '0600'
    owner: forge:forge
    defer: true
    content: |
      {{.GitCredentialURL}}
{{- end}}

  # NOTE: the Claude Code statusline used to live here as a deferred
  # write_files entry, but cloud-init 25.x in Debian trixie wedges
  # cloud-final indefinitely when the deferred-writes set targets a
  # parent dir that doesn't exist yet (/home/forge/.claude). Bootstrap
  # writes both files itself now (see /usr/local/bin/forge-bootstrap),
  # which keeps cloud-init's userdata smaller and more importantly
  # keeps modules-config → modules-final from getting stuck.

  # Append a TERM fallback to forge's .bashrc. Ghostty's terminfo entry
  # (xterm-ghostty) isn't shipped with Debian — without this, every
  # interactive command inside the VM prints "WARNING: terminal is not
  # fully functional" and stalls behind a less pager prompt. append:true
  # keeps whatever skel put in the default .bashrc (history controls,
  # ll aliases, etc).
  - path: /home/forge/.bashrc
    owner: forge:forge
    defer: true
    append: true
    content: |

      # Fall back to xterm-256color on hosts that don't have the ghostty terminfo entry
      case "$TERM" in
          xterm-ghostty) export TERM=xterm-256color ;;
      esac
{{- if .GitUserName}}

  # forge user's ~/.gitconfig. Written here (rather than via
  # 'sudo -u forge -H git config --global' in runcmd) so cloud-init
  # owns the file and we get deterministic ownership: an earlier
  # runcmd-based approach left /home/forge/.gitconfig unwritable by
  # the forge user, blocking the first commit. defer:true ensures the
  # forge user (and home dir) exist when this runs.
  - path: /home/forge/.gitconfig
    permissions: '0644'
    owner: forge:forge
    defer: true
    content: |
      [user]
          name = {{.GitUserName}}
          email = {{.GitUserEmail}}
{{- if .ForgejoHostBase}}
      [credential]
          helper = store
{{- end}}
{{- end}}

  - path: /usr/local/bin/forge-ready
    permissions: '0755'
    content: |
      #!/usr/bin/env python3
      # Signals 'forge env create' that bootstrap finished. Connects to
      # the host's vsock listener at CID=2 (VMADDR_CID_HOST) port 1234
      # and sends a single ready line.
      import socket, subprocess, sys
      ip = subprocess.check_output(["hostname", "-I"]).decode().strip().split()[0]
      s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
      s.connect((2, 1234))
      s.sendall(f"ready addr={ip}\n".encode())
      s.close()

  - path: /etc/systemd/system/forge-ssh-vsock.service
    permissions: '0644'
    content: |
      # Bridges incoming vsock:22 connections to the local sshd. Combined
      # with vfkit's virtio-vsock device on port 22 (socketURL pointing at a
      # host-side Unix socket), this lets the host ssh into the VM via that
      # socket — completely bypassing the IP routing table, which a
      # corporate VPN may have hijacked. socat is pre-installed in the
      # FORGE base image.
      [Unit]
      Description=FORGE host-SSH vsock bridge
      After=ssh.service
      Wants=ssh.service

      [Service]
      Type=simple
      ExecStart=/usr/bin/socat VSOCK-LISTEN:22,fork,reuseaddr TCP:127.0.0.1:22
      Restart=always
      RestartSec=2s

      [Install]
      WantedBy=multi-user.target
{{- if .ForgejoVsockPort}}

  - path: /etc/systemd/system/forge-forgejo-vsock.service
    permissions: '0644'
    content: |
      # Reverse of forge-ssh-vsock: listens on 127.0.0.1:{{.ForgejoVsockPort}}
      # inside the VM and forwards each connection over vsock to the
      # host (CID=2, port={{.ForgejoVsockPort}}). vfkit's matching
      # virtio-vsock,listen device dials a unix socket on the host;
      # internal/forgejoproxy is what binds that socket and bridges to
      # the real Forgejo TCP endpoint. The whole thing exists because a
      # IP-routing-free path keeps git push working even if the
      # userspace netstack (gvproxy) ever hits an issue — vsock never
      # touches the IP stack.
      [Unit]
      Description=FORGE Forgejo vsock bridge

      [Service]
      Type=simple
      ExecStart=/usr/bin/socat TCP-LISTEN:{{.ForgejoVsockPort}},bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:{{.ForgejoVsockPort}}
      Restart=always
      RestartSec=2s

      [Install]
      WantedBy=multi-user.target
{{- end}}

  - path: /usr/local/bin/forge-bootstrap
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      # Args: <k3s_version> <rage_version> <claude_version> <helm_version>
      set -euo pipefail
      exec >> /var/log/forge-bootstrap.log 2>&1
      echo "[$(date -Is)] forge-bootstrap starting"

      K3S_VERSION="$1"
      RAGE_VERSION="$2"
      CLAUDE_VERSION="$3"
      HELM_VERSION="$4"

      # Verify required commands BEFORE doing anything else. Bootstrap
      # assumes a small set of tools were installed at base-image
      # build time (images/base/build.sh's --install line). When the
      # base image drifts out of sync with the bootstrap script, the
      # symptom can be subtle — e.g. a missing 'jq' silently breaks
      # the claude statusline while everything else "works." Failing
      # here gives a clear error AND makes 'forge env create' time
      # out fast → status: crashed, instead of bootstrap hanging in
      # some half-installed state. Keep this list in lock-step with
      # images/base/build.sh.
      require_cmd() {
          local missing=()
          for cmd in "$@"; do
              if ! command -v "$cmd" >/dev/null 2>&1; then
                  missing+=("$cmd")
              fi
          done
          if [ "${#missing[@]}" -gt 0 ]; then
              echo "ERROR: required commands missing from base image: ${missing[*]}" >&2
              echo "       Rebuild the base image (images/base/build.sh) to include them." >&2
              echo "       This is a hard-stop because forge-bootstrap depends on these." >&2
              exit 1
          fi
      }
      require_cmd curl jq git socat sudo install tar base64 sha256sum systemctl python3

      # Cloud-init reaches modules:final before networkd has finished DHCP
      # on slow first boots. Wait up to 60s for a default route.
      for _ in $(seq 1 60); do
          ip route | grep -q '^default' && break
          sleep 1
      done
      ip route | grep -q '^default' || { echo "no default route after 60s"; exit 1; }

      # Create a 2 GB swap file BEFORE memory-heavy installs. The
      # claude-code native binary is ~250 MB and its 'install'
      # subcommand allocates further during launcher setup; with the
      # 4 GB forge.yaml default and k3s already resident, the install
      # step gets SIGKILL'd by the OOM killer ("Killed" with no other
      # message). 2 GB swap absorbs the spike on the way in and stays
      # around to give the running agent + k3s pods breathing room.
      # /etc/fstab entry preserves it across reboots.
      if [ ! -f /var/swap.img ]; then
          # fallocate is fast on ext4 but some filesystems reject it
          # for swap (must be a real allocated file, not sparse). Fall
          # back to dd if fallocate-then-mkswap fails.
          fallocate -l 2G /var/swap.img \
              || dd if=/dev/zero of=/var/swap.img bs=1M count=2048 status=none
          chmod 0600 /var/swap.img
          mkswap /var/swap.img >/dev/null
          swapon /var/swap.img
          echo "/var/swap.img none swap sw 0 0" >> /etc/fstab
      fi

      # Pre-create /home/forge/.config owned by forge BEFORE any install
      # step that might create it as root. GNU 'install -d -o forge -g
      # forge /home/forge/.config/<sub>' creates intermediate parents
      # with default ownership (root:root) and only applies -o/-g to
      # the leaf — without this fix, .config ends up unwritable to the
      # forge user, breaking 'helm repo add', kubectl plugin caches,
      # and any other tool that uses XDG_CONFIG_HOME.
      install -d -o forge -g forge -m 0755 /home/forge/.config

      # /home/forge/.claude needs the same treatment: cloud-init's
      # write_files (with defer:true) creates intermediate parent dirs
      # but applies the entry's owner only to the leaf file, leaving
      # /home/forge/.claude root-owned. Without the explicit pre-
      # create, the very first 'claude install' (run as forge later
      # in this script) hits "permission denied" trying to write
      # ~/.claude/downloads.
      install -d -o forge -g forge -m 0755 /home/forge/.claude

      # Drop the embedded Claude Code statusline + minimal settings
      # into /home/forge/.claude. Done HERE in bash rather than via
      # cloud-init's write_files (defer:true) because cloud-init 25.x
      # in Debian trixie wedges modules-final indefinitely on
      # deferred writes that target parent dirs which don't yet
      # exist — the symptom was a forever-running cloud-init-main
      # parked at "Waiting on external services to complete before
      # starting the final stage." The statusline blob is base64-
      # encoded by the Go template at render time so this script
      # only deals with safe ASCII. See
      # internal/cloudinit/files/statusline.sh for the source.
      base64 -d > /home/forge/.claude/statusline.sh <<'STATUSLINE_B64'
      {{.StatuslineB64}}
      STATUSLINE_B64
      cat > /home/forge/.claude/settings.json <<'SETTINGS_JSON'
      {
        "statusLine": {
          "type": "command",
          "command": "~/.claude/statusline.sh"
        }
      }
      SETTINGS_JSON
      chown forge:forge /home/forge/.claude/statusline.sh /home/forge/.claude/settings.json
      chmod 0755 /home/forge/.claude/statusline.sh
      chmod 0644 /home/forge/.claude/settings.json

      # K3S_KUBECONFIG_MODE=644 makes /etc/rancher/k3s/k3s.yaml world-
      # readable so the forge user (and the in-VM agent running as
      # forge) can run kubectl without sudo. Same trust boundary as
      # the rest of the VM — single-tenant; the user already has
      # sudo NOPASSWD via cloud-init users.
      #
      # The installer script is pinned by sha256 too: the k3s binary
      # itself is verified by the script (against k3s release
      # SHA256SUMS), but the script blob is what we exec, so pin it.
      K3S_INSTALLER="/tmp/k3s-install.sh"
      curl -fsSL https://get.k3s.io -o "$K3S_INSTALLER"
      echo "` + k3sInstallerSHA256 + `  $K3S_INSTALLER" | sha256sum -c -
      INSTALL_K3S_VERSION="$K3S_VERSION" \
          K3S_KUBECONFIG_MODE=644 \
          sh "$K3S_INSTALLER"
      rm -f "$K3S_INSTALLER"

      # Point kubectl at k3s's config by default. Login shells source
      # /etc/profile.d, so any 'forge env connect' (or interactive
      # ssh) sees KUBECONFIG set without extra env-var juggling.
      cat > /etc/profile.d/k3s.sh <<'PROFILE'
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
      PROFILE
      chmod 0755 /etc/profile.d/k3s.sh

      # helm — Kubernetes package manager. Same pattern as k3s: pre-
      # download the official installer (helm/helm/scripts/get-helm-3),
      # verify its sha256 against our pin, then exec with
      # DESIRED_VERSION pointing at the forge.yaml-supplied version.
      # The installer downloads helm-<version>-linux-arm64.tar.gz from
      # helm's release server and verifies it against helm's published
      # SHA256SUMS automatically; our pin guards the wrapper itself.
      # HELM_INSTALL_DIR defaults to /usr/local/bin, which puts helm on
      # the default PATH for both the forge user and any non-login
      # child process — no profile.d entry needed.
      HELM_INSTALLER="/tmp/get-helm-3"
      curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 -o "$HELM_INSTALLER"
      echo "` + helmInstallerSHA256 + `  $HELM_INSTALLER" | sha256sum -c -
      DESIRED_VERSION="$HELM_VERSION" bash "$HELM_INSTALLER"
      rm -f "$HELM_INSTALLER"

      # Rage is user-provided, NOT fetched from the network. The host
      # exposes ~/.forge/rage as a read-only virtio-fs mount with the
      # tag rage-share; we copy the platform-appropriate binary into
      # /usr/local/bin/rage and rage.toml into /home/forge/.config/rage.
      # See docs/cage-README.md for the rage/ directory convention.
      mkdir -p /mnt/rage-host
      modprobe virtiofs 2>/dev/null || true
      if mount -t virtiofs -o ro rage-share /mnt/rage-host 2>/dev/null; then
          ARCH=$(uname -m)
          RAGE_SRC="/mnt/rage-host/rage-${ARCH}-linux"
          if [ -f "$RAGE_SRC" ]; then
              install -m 0755 "$RAGE_SRC" /usr/local/bin/rage
              echo "Installed rage: $(/usr/local/bin/rage --version 2>&1 || echo unknown) (forge.yaml pin: $RAGE_VERSION)"
          else
              echo "WARNING: no rage binary for ${ARCH} in rage-share; rage will not be available"
          fi
          if [ -f /mnt/rage-host/rage.toml ]; then
              install -d -o forge -g forge -m 0700 /home/forge/.config/rage
              install -o forge -g forge -m 0644 /mnt/rage-host/rage.toml /home/forge/.config/rage/rage.toml
          fi
          umount /mnt/rage-host
      else
          echo "WARNING: rage-share virtiofs not attached; skipping rage install. Run 'forge init' with a rage/ dir on the host to enable."
      fi

      # claude-code is installed via Anthropic's official installer
      # (https://claude.ai/install.sh, documented at code.claude.com/docs).
      # The script downloads a single-file native binary — no Node.js,
      # no npm, no pnpm needed. We pre-download the script and verify
      # its content sha256 BEFORE executing, so a swapped installer
      # cannot run with our trust.
      #
      # install.sh accepts a TARGET arg matching ^(stable|latest|N.N.N)$.
      # forge.yaml's bootstrap.claude_code value lands here; strip a
      # leading 'v' so v0.4.2-style pins (consistent with k3s/rage)
      # still satisfy the regex.
      CLAUDE_INSTALLER="/tmp/claude-install.sh"
      curl -fsSL https://claude.ai/install.sh -o "$CLAUDE_INSTALLER"
      echo "` + claudeCodeInstallerSHA256 + `  $CLAUDE_INSTALLER" | sha256sum -c -
      CLAUDE_TARGET="${CLAUDE_VERSION#v}"

      # Run as the forge user with HOME=/home/forge. install.sh writes
      # to $HOME/.claude/downloads/ during the bootstrap-binary fetch
      # and 'claude install' lands the persistent binary in
      # $HOME/.local/bin/claude — both must end up under forge so the
      # in-VM agent (running as forge) can read/exec them.
      #
      # Pre-create ~/.local/bin and inline it on PATH for the install
      # itself: claude install does a post-step PATH check and warns
      # if the dir isn't already there. Sudo resets PATH to a minimal
      # default that doesn't include ~/.local/bin, hence the explicit
      # env override. Future login shells pick the dir up via
      # Debian's default ~/.profile snippet (the 'if [ -d ~/.local/bin ]' branch).
      sudo -u forge -H mkdir -p /home/forge/.local/bin
      sudo -u forge -H env "PATH=/home/forge/.local/bin:$PATH" \
          bash "$CLAUDE_INSTALLER" "$CLAUDE_TARGET"
      rm -f "$CLAUDE_INSTALLER"

      # Make claude discoverable to all users and to non-login child
      # processes (rage spawns claude via the user's PATH; without a
      # login shell, $HOME/.local/bin isn't on PATH). Symlink into
      # /usr/local/bin so it works regardless of shell init plumbing.
      if [ -x /home/forge/.local/bin/claude ]; then
          ln -sf /home/forge/.local/bin/claude /usr/local/bin/claude
      else
          echo "WARNING: claude not found at /home/forge/.local/bin/claude after install"
      fi
      echo "[$(date -Is)] forge-bootstrap done"

runcmd:
  # Defensively re-chown /home/forge. When the users block has an
  # explicit numeric UID set AND deferred write_files target
  # /home/forge/*, cloud-init on Debian 13 cloud-arm64 has been
  # observed to leave the directory itself owned by root:root mode
  # 0755 — so the forge user can read and traverse, but cannot create
  # the *.lock files git's credential helper needs for "git push"
  # post-success "approve" updates. The symptoms are confusing
  # because the push still works (creds are readable) but git prints
  # a fatal-looking message about a credential storage lock timeout.
  # -R fixes the dir AND any write_files entries cloud-init wrote
  # into it.
  - chown -R forge:forge /home/forge
  - systemctl daemon-reload
  - systemctl enable --now forge-ssh-vsock.service
{{- if .ForgejoVsockPort}}
  - systemctl enable --now forge-forgejo-vsock.service
{{- end}}
  # Chain bootstrap and forge-ready with shell '&&' so a failed
  # bootstrap actually fails the env create. Cloud-init's runcmd
  # entries run sequentially regardless of exit status — without the
  # chain, a bootstrap that bailed mid-way (e.g. claude install.sh
  # checksum fails) would still let forge-ready signal "ready" and
  # 'forge env create' would falsely report success.
  - /usr/local/bin/forge-bootstrap "{{.K3sVersion}}" "{{.RageVersion}}" "{{.ClaudeCodeVersion}}" "{{.HelmVersion}}" && /usr/local/bin/forge-ready
`

// NetworkConfigInput is the static-IP description rendered into the
// cidata network-config. DHCP4 is intentionally not supported — vfkit's
// vmnet shared mode relies on macOS's launchd-socket-activated bootpd,
// which does not wake reliably when other vmnet consumers (OrbStack,
// other VMs) are running concurrently. Static IPs sidestep that
// failure mode entirely.
type NetworkConfigInput struct {
	// Address is dotted-decimal, e.g. "192.168.127.42".
	Address string
	// Prefix is the CIDR prefix length, e.g. 24.
	Prefix int
	// Gateway is dotted-decimal, e.g. "192.168.127.1".
	Gateway string
	// DNS is a list of DNS server addresses; rendered into nameservers.
	DNS []string
}

const networkConfigStaticTemplate = `version: 2
ethernets:
  enp0s1:
    addresses: [{{.Address}}/{{.Prefix}}]
    nameservers:
      addresses: [{{range $i, $d := .DNS}}{{if $i}}, {{end}}{{$d}}{{end}}]
    routes:
      - to: default
        via: {{.Gateway}}
`

// RenderUserData renders the user-data file as bytes, ready to be written into
// a NoCloud ISO. The leading "#cloud-config" header is included.
func RenderUserData(in UserDataInput) ([]byte, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("cloudinit: name is required")
	}
	if in.AuthorizedKey == "" {
		return nil, fmt.Errorf("cloudinit: authorized key is required")
	}
	if in.K3sVersion == "" || in.RageVersion == "" || in.ClaudeCodeVersion == "" || in.HelmVersion == "" {
		return nil, fmt.Errorf("cloudinit: bootstrap versions are required (k3s, rage, claude_code, helm)")
	}
	if in.GitUserName != "" && in.GitUserEmail == "" {
		return nil, fmt.Errorf("cloudinit: git user email required when name is set")
	}

	// Compute the credentials URL once so the template only deals with
	// strings. We only emit the .git-credentials block when the full
	// trio (host base, user, token) is available. The base used here
	// is the same one that ends up in `git remote -v` because the
	// in-VM socat unit makes localhost:<port> resolve through vsock to
	// the host-side proxy.
	data := struct {
		UserDataInput
		GitCredentialURL string
		StatuslineB64    string
	}{
		UserDataInput: in,
		StatuslineB64: base64.StdEncoding.EncodeToString(statuslineSh),
	}
	if in.ForgejoHostBase != "" && in.ForgejoUser != "" && in.ForgejoToken != "" {
		credURL, err := injectAuthIntoURL(in.ForgejoHostBase, in.ForgejoUser, in.ForgejoToken)
		if err != nil {
			return nil, fmt.Errorf("forming git credential URL: %w", err)
		}
		data.GitCredentialURL = credURL
	}

	tmpl, err := template.New("userdata").Parse(userDataTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing user-data template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering user-data: %w", err)
	}
	return buf.Bytes(), nil
}

// injectAuthIntoURL returns rawURL with user:token embedded in the
// userinfo, e.g. http://host:4000 + (forge, abc) → http://forge:abc@host:4000.
func injectAuthIntoURL(rawURL, user, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, token)
	return u.String(), nil
}

// RenderMetaData renders the cloud-init meta-data file. Just the
// instance-id and hostname — that's all NoCloud requires.
func RenderMetaData(name string) []byte {
	return fmt.Appendf(nil, "instance-id: forge-%s\nlocal-hostname: forge-%s\n", name, name)
}

// RenderNetworkConfig returns the cidata network-config blob. NoCloud
// reads this to know how to bring up interfaces; without it, Debian 13
// cloud-arm64 leaves the virtio-net device unconfigured (no IPv4, no
// default route).
func RenderNetworkConfig(in NetworkConfigInput) ([]byte, error) {
	if in.Address == "" {
		return nil, fmt.Errorf("cloudinit: network address is required")
	}
	if in.Prefix == 0 {
		return nil, fmt.Errorf("cloudinit: network prefix is required")
	}
	if in.Gateway == "" {
		return nil, fmt.Errorf("cloudinit: network gateway is required")
	}
	if len(in.DNS) == 0 {
		return nil, fmt.Errorf("cloudinit: at least one DNS server is required")
	}

	tmpl, err := template.New("netcfg").Parse(networkConfigStaticTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing network-config template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return nil, fmt.Errorf("rendering network-config: %w", err)
	}
	return buf.Bytes(), nil
}
