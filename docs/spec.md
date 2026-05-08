# FORGE MVP Specification

**F**ederated **O**rchestrated **R**untime **G**uarded **E**nvironment

> VM-based sandbox for running AI coding agents (Claude Code) in isolation, with native Kubernetes support.

---

## 1. Background & motivation

CAGE provides isolated Docker containers for AI coding sessions. Docker works well for general tasks but cannot run Kubernetes properly — it requires privileged containers, nested namespaces, and a shared kernel, which undermines the security model.

FORGE solves this by replacing containers with virtual machines, providing:

- Hypervisor-level kernel isolation
- Native Kubernetes (k3s running directly in the VM)
- The same CLI-driven developer experience as CAGE

FORGE is a standalone project that follows CAGE's patterns and philosophy.

---

## 2. Decision log

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Hypervisor interface | Shell out to `vfkit` binary | Same pattern as CAGE→Docker; keeps FORGE pure Go; no CGo/Swift at build time |
| 2 | Image distribution | GitHub Releases, behind `ImageSource` interface | Team's central OCI registry is in progress; interface makes migration a one-file change |
| 3 | Image composition | Thin base (Debian arm64 + OS tooling only) | FORGE owns only what it builds; RAGE/k3s/Claude Code have active development and independent SBOMs. Default Debian major version is configurable via `DEBIAN_VERSION` (currently 13/trixie) |
| 4 | Bootstrap trigger | cloud-init userdata generated at `forge env create` | Standard, battle-tested; version pins live in config not the image |
| 5 | VM process management | PID + `Setsid`, explicit state machine, no auto-restart | Session-scoped VMs; devs decide when to restart, not the tool |
| 6 | Network | Userspace TCP/IP stack on the host (`gvisor-tap-vsock`, subnet `192.168.127.0/24`) reached via vfkit's `virtio-net,unixSocketPath`. SSH and Forgejo ride dedicated vsock channels. | Apple VF's vmnet shared mode is incompatible with tunnel-all corp VPNs (NAT'd packets get dropped by the NEPacketTunnelProvider). gvproxy maps every VM connection to a host-side `socket()` call, indistinguishable from a Mac browser to the policy engine. Host↔VM control paths use vsock + unix sockets so no path depends on macOS routing. |
| 7 | SSH keys | Per-env ed25519 keypair by default; user key opt-in via config | Env-specific keys auto-invalidate on destroy; no `~/.ssh/config` pollution |
| 8 | Forgejo | FORGE manages its own Docker container; configurable to external | Standalone by default; teams sharing CAGE's Forgejo or a team instance just set `forgejo.url` |
| 9 | `forge env connect` | RAGE/Claude Code session by default; `--bash` for shell | Mirrors CAGE exactly — drop straight into the agent session |
| 10 | Resource defaults | 2 vCPU / 4 GB RAM / 20 GB disk | Conservative defaults for Apple Silicon hosts; all overridable |
| 11 | Config format | `forge.yaml` (project-level) + `~/.forge/config.yaml` (global) | Project settings (versions, resources) belong in the repo; machine settings (credentials, paths) are personal |
| 12 | `forge env bake` | Out of scope | Full VMs — developers install packages directly; no snapshotting needed |

---

## 3. MVP scope

### In scope

- Boot and manage Debian arm64 VMs via vfkit on macOS Apple Silicon (default Debian 13)
- k3s single-node Kubernetes running inside each VM
- `forge init` to scaffold a project-level `forge.yaml`; upward-walk discovery + built-in defaults for unconfigured directories
- `forge env` lifecycle commands: create, connect, list, start, stop, destroy
- `forge system` commands: start, stop, status, disconnect (Forgejo)
- `forge image` commands: pull, list, import
- cloud-init bootstrap: k3s, RAGE, Claude Code, helm at pinned versions
- Per-env SSH keypair management
- Forgejo Docker container managed by FORGE, configurable to external
- Per-env Forgejo user + `workspace` repo (mirrors CAGE)
- virtio-vsock boot-complete signalling
- Two-level config: `forge.yaml` + `~/.forge/config.yaml`
- Pinned Forgejo and Debian-builder image digests (14-day soak rule, refreshed by `scripts/refresh-pins.sh`)
- SBOM published alongside base image on GitHub Releases

### Out of scope (post-MVP)

- Linux / KVM support
- OCI registry image distribution (interface is ready, implementation deferred)
- `forge env bake`
- Multi-node k3s clusters
- Language packs
- Cloud VM backends
- Web UI
- `forge env snapshot`

---

## 4. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Host (macOS Apple Silicon)                                      │
│                                                                  │
│  ┌─────────────┐                                                 │
│  │  Forgejo    │──┐ vsock (forgejoproxy)                         │
│  │  (Docker)   │  │ host-unix → vfkit → guest-vsock              │
│  │  Git review │  ▼                                              │
│  │  gate       │ ┌──────────────────────────────────┐            │
│  └─────────────┘ │  VM (Debian 13 arm64)            │            │
│                  │                                  │            │
│  ┌─────────────┐ │  ┌────────────────────────────┐  │            │
│  │  FORGE CLI  │ │  │  k3s (single-node)         │  │            │
│  │  (Go/cobra) │ │  │  ┌──────────────────────┐  │  │            │
│  └─────────────┘ │  │  │  Workloads           │  │  │            │
│         │        │  │  └──────────────────────┘  │  │            │
│         │ vsock  │  └────────────────────────────┘  │            │
│         └───────►│  RAGE → Claude Code              │            │
│         (SSH)    │  openssh-server                  │            │
│                  │  cloud-init (bootstrap)          │            │
│                  └─────────────┬────────────────────┘            │
│                                │ virtio-net via unixgram socket  │
│                                ▼                                 │
│                  ┌──────────────────────────────────┐            │
│                  │  gvproxy (gvisor-tap-vsock)      │            │
│                  │  192.168.127.0/24, DHCP+DNS+NAT  │            │
│                  │  per-conn host socket() calls    │            │
│                  └─────────────┬────────────────────┘            │
│                                │                                 │
│                                ▼ macOS network stack             │
│                            (internet, with corp VPN if any)      │
└──────────────────────────────────────────────────────────────────┘
```

Three independent host↔guest channels per env: SSH (vsock), Forgejo
(vsock), and general egress (gvproxy / userspace TCP/IP). None of them
traverse the host's IP routing table to reach the VM, so all three work
with a tunnel-all corp VPN connected. See § 9 for the wire-level details.

### External dependencies

| Dependency | How obtained | Purpose |
|------------|-------------|---------|
| `vfkit` | `brew install vfkit` | Apple Virtualization.framework wrapper |
| A working `docker` CLI | Any backend with a Linux daemon | Forgejo container runtime |
| `cloud-init` tooling | Bundled in base image | Per-env provisioning |

FORGE doesn't care which Docker backend the operator runs; it only needs `docker` and `docker compose` on PATH with a reachable daemon. The README recommends `brew install docker` + `brew install colima` as the license-clean default, but anything that gives `docker ps` a working daemon to talk to is fine.

---

## 5. CLI reference

### `forge init [path]`

Writes a default `forge.yaml` into the given directory (or CWD if no path is given) so that `forge env` commands can be run there without depending on the FORGE git repository. Refuses if `forge.yaml` already exists; `--force` overwrites.

The content matches the embedded defaults FORGE falls back to when no `forge.yaml` is found, so a freshly-`init`-ed file is identical to the implicit zero-config behaviour. Users only need to run `init` when they want to customise bootstrap pins or resource defaults and check the result into version control.

### Project-config discovery

`forge env create` and `forge env start` find their `forge.yaml` by walking upward from the current directory: CWD first, then each parent up to the filesystem root, taking the first match. With no match anywhere, the embedded defaults are used. The chosen source (file path or `built-in defaults`) is logged at debug level so `--debug` can disambiguate when needed.

`forge env create` adds a guard against silently inheriting defaults you didn't notice: when no `forge.yaml` is found and stdin is a TTY, it prompts to initialise the current directory before continuing. Answering `n` falls back to built-in defaults; non-interactive runs (no TTY) fail with a hint to either run `forge init` first or pass `--no-init` to skip the prompt explicitly. `forge env start` does not prompt — by then an env already exists, so blocking on a prompt would be too late to be useful.

### `forge env create [name]`

Creates and starts a new VM environment.

1. Reads `forge.yaml` for bootstrap versions and resource config
2. Copies base image from `~/.forge/images/` to `~/.forge/envs/<name>/disk.img`
3. Generates per-env ed25519 SSH keypair
4. Generates cloud-init ISO with: SSH authorized key, hostname, bootstrap script (k3s + RAGE + Claude Code at pinned versions)
5. Starts vfkit as a detached process (`Setsid`), writes PID to `~/.forge/envs/<name>/vfkit.pid`
6. Listens on virtio-vsock for boot-complete signal from VM
7. Writes VM IP and state to `~/.forge/envs/<name>/state.json`
8. Registers VM as a Forgejo remote (creates repo if needed)
9. Prints connection instructions

Flags:
- `--cpus int` — override vCPU count (default: 2)
- `--memory int` — override RAM in MB (default: 4096)
- `--disk int` — override disk in MB (default: 20480)
- `--image string` — use specific image version (default: latest cached)

### `forge env connect [name]`

Connects to an environment. Accepts envs in either `running` or
`starting` state — the latter so the user can SSH in mid-bootstrap to
`tail -f /var/log/forge-bootstrap.log` while a long install (e.g. the
claude-code installer fetching the native binary) is still running.

Default: SSHes into the VM and launches RAGE, which spawns the Claude
Code session. Mirrors `cage env connect`.

- `--bash` — drop to an interactive shell instead of launching RAGE.

Both forms wrap the remote command in `bash -l -c 'cd
/home/forge/workspace && exec <cmd>'`:

- The login bash sources `/etc/profile.d/*.sh`, which exports
  `KUBECONFIG` for k3s. The claude binary itself lives at
  `/usr/local/bin/claude` (symlinked there during bootstrap from
  `/home/forge/.local/bin/claude` where `claude.ai/install.sh`
  places it), so it's reachable on the default PATH whether or not
  shell init has sourced. The login bash is still required for
  KUBECONFIG.
- `cd /home/forge/workspace` lands the agent inside the env's
  Forgejo project (the virtio-fs share), so `git push` from the
  agent goes to the right place. Without it, Claude Code launches
  in `$HOME` and warns about it.
- `exec <cmd>` replaces bash so there's no stray parent process.

### `forge env list`

Lists all environments with live status. Checks each PID to report accurate state (stale PIDs → `crashed`).

```
NAME        STATUS    IP                CPUS  MEM   CREATED
my-project  running   192.168.127.42    2     4096  2h ago
old-env     stopped   192.168.127.43    2     4096  3d ago
broken-env  crashed   192.168.127.44    2     4096  1d ago
```

### `forge env start [name]`

Starts a stopped or crashed environment. Reuses existing disk, generates fresh cloud-init ISO.

### `forge env stop [name]`

Gracefully shuts down the VM. Disk is preserved. State → `stopped`.

### `forge env destroy [name]`

Stops the VM and deletes all env state: disk, keys, cloud-init ISO, state file.

### `forge system start`

Sets up FORGE's connection to a Forgejo instance. Two modes:

- **Existing** (`--mode existing`): connect to an already-running Forgejo (e.g. CAGE's, or a team-shared instance). Prompts for URL, admin user, and password (or reads `FORGE_ADMIN_PASSWORD` from env). Verifies admin scope, provisions a `forge-cli` API token, persists URL + token to `~/.forge/config.yaml`. No Docker container is started.
- **New** (`--mode new`): start a FORGE-managed Forgejo container. Probes from port 3000 upward for a free port, prompts for admin user (default `forge`) and password, runs the container at the pinned digest (`internal/forgejo/pins.go`), creates the admin user and API token, persists everything to `~/.forge/config.yaml`. The password is the user's to remember (web-UI login); FORGE only stores the token.

Without `--mode`, prompts interactively. `--force` re-runs setup against an already-configured connection.

### `forge system stop`

Stops the FORGE-managed Forgejo container. No-op when FORGE is using an external Forgejo (`forgejo.url` set).

### `forge system status`

Prints health of: Forgejo (reachable, mode), vfkit (installed, correct version), image cache (latest image present). Works in both managed and external modes.

### `forge system disconnect`

Clears the Forgejo block from `~/.forge/config.yaml` so `forge system start` can configure a fresh connection (rotate token, switch instance). Config-only — does not stop a managed container. Run `stop` first if both are needed. Works in both modes.

### `forge image pull [version]`

Downloads a base image from GitHub Releases to `~/.forge/images/`. Defaults to latest release. Verifies checksum. Also downloads the accompanying SBOM.

### `forge image import <path>`

Imports a locally-built (or otherwise-obtained) base image into the cache. The entry point for local development: `images/base/build-in-docker.sh` produces an image and `forge image import` makes it available to `forge env create`.

### `forge image list`

Lists locally cached base images with version and size.

---

## 6. VM lifecycle state machine

```
          forge env create
                │
                ▼
           [creating]
                │ vfkit started, cloud-init running
                ▼
           [starting]
                │ vsock boot-complete signal received
                ▼
           [running] ◄─── forge env start
                │
         ┌──────┴──────┐
         │              │
   forge env stop    vfkit process dies unexpectedly
         │              │
         ▼              ▼
      [stopped]      [crashed]
         │              │
    forge env destroy   forge env start / forge env destroy
         │
         ▼
      [destroyed] (all files removed)
```

`forge env list` detects `crashed` lazily: if `vfkit.pid` exists but the process is not alive, state is `crashed`.

---

## 7. Base image

### Contents (thin base — FORGE owns)

- Debian arm64 minimal (default Debian 13 / trixie; major version configurable via `DEBIAN_VERSION` at build time)
- openssh-server
- cloud-init
- curl, git, ca-certificates, sudo
- virtio-vsock kernel module enabled
- A `forge-ready` systemd service that sends the boot-complete vsock signal

### What is NOT in the base image

- k3s
- RAGE
- Claude Code
- Language tooling

These are installed by cloud-init at `forge env create` time, at versions pinned in `forge.yaml`.

### Image publishing (CI)

1. Build image using a Packer template (or shell script + QEMU)
2. Generate SBOM with `syft` in SPDX and CycloneDX formats
3. Publish to GitHub Releases: `forge-base-<version>-arm64.img.gz`, `sbom.spdx.json`, `sbom.cdx.json`, `SHA256SUMS`
4. External scanners (Grype, Trivy) consume the published image and SBOM

### `ImageSource` interface

The download logic is behind a Go interface to make OCI migration trivial:

```go
type ImageSource interface {
    LatestVersion(ctx context.Context) (string, error)
    Pull(ctx context.Context, version, destPath string) error
    ListVersions(ctx context.Context) ([]string, error)
}
```

MVP implementation: `GitHubReleasesSource`. Future: `OCIRegistrySource`.

---

## 8. Bootstrap (cloud-init)

FORGE generates a NoCloud cloud-init ISO at `forge env create` time
containing user-data, meta-data, and network-config. The user-data
template is `internal/cloudinit/userdata.go`. The salient bits:

```yaml
#cloud-config
hostname: forge-<name>

users:
  - name: forge
    uid: <host UID>           # matches os.Getuid() so virtio-fs ownership lines up
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - <per-env public key>

# Workspace virtio-fs share auto-mounts on every boot via fstab.
mounts:
  - [ workspace-share, /home/forge/workspace, virtiofs, "defaults,nofail", "0", "0" ]

write_files:
  # forge-ssh-vsock.service: socat VSOCK-LISTEN:22 → TCP:127.0.0.1:22
  # forge-forgejo-vsock.service: socat TCP-LISTEN:<port>,bind=127.0.0.1
  #                                    → VSOCK-CONNECT:2:<port>
  # /home/forge/.gitconfig: pre-seeded user.name + user.email +
  #                         credential helper (defer:true)
  # /home/forge/.git-credentials: forge:<token>@<forgejo-host> (defer:true)
  # /home/forge/.bashrc append: TERM=xterm-256color fallback for ghostty
  # /usr/local/bin/forge-bootstrap: the install script (see below)
  # /usr/local/bin/forge-ready: vsock-1234 ready signal
  - …

runcmd:
  - chown -R forge:forge /home/forge        # Debian-cloud-arm64 quirk
  - systemctl daemon-reload
  - systemctl enable --now forge-ssh-vsock.service
  - systemctl enable --now forge-forgejo-vsock.service
  - /usr/local/bin/forge-bootstrap "$K3S_VERSION" "$CLAUDE_VERSION" "$HELM_VERSION" \
      && /usr/local/bin/forge-ready
```

The `forge-bootstrap` script (also written by `write_files`) does the
heavy lifting with `set -euo pipefail`. In order:

1. Wait up to 60s for a default route (cloud-init `final` runs before
   networkd has finished DHCP on slow first boots).
2. Pre-fetch the k3s install script, verify its sha256 against
   `k3sInstallerSHA256`, then run with `INSTALL_K3S_VERSION=$K3S_VERSION
   K3S_KUBECONFIG_MODE=644 sh` so `/etc/rancher/k3s/k3s.yaml` is
   readable by the forge user.
3. Write `/etc/profile.d/k3s.sh` exporting `KUBECONFIG`.
4. Pre-fetch helm's `get-helm-3` script, verify its sha256 against
   `helmInstallerSHA256`, then run with `DESIRED_VERSION=$HELM_VERSION
   bash get-helm-3`. The installer downloads
   `helm-<version>-linux-arm64.tar.gz` from helm's release server and
   verifies it against helm's published SHA256SUMS automatically; our
   pin guards the wrapper itself. Lands in `/usr/local/bin/helm`.
5. Mount the `rage-share` virtio-fs share (read-only) and copy
   `rage-$(uname -m)-linux` to `/usr/local/bin/rage`, `rage.toml` to
   `/home/forge/.config/rage/rage.toml`. Rage is **never** fetched
   over the network — the user provides the binary per the CAGE
   convention.
6. Pre-fetch `https://claude.ai/install.sh`, verify its sha256
   against `claudeCodeInstallerSHA256`, then run as the forge user:
   `sudo -u forge -H bash claude-install.sh "${CLAUDE_VERSION#v}"`.
   The script downloads a single-file native binary (verified
   against claude.ai's `manifest.json`) and runs `claude install
   <TARGET>` to set up the persistent launcher in
   `/home/forge/.local/bin/claude`. No Node.js, no npm, no pnpm in
   the bootstrap path. The leading-`v` strip on `CLAUDE_VERSION` is
   defensive — install.sh's TARGET regex rejects v-prefixed
   versions but forge.yaml entries written as `v0.4.2` (k3s/rage
   convention) should still work.
7. Symlink `/home/forge/.local/bin/claude` → `/usr/local/bin/claude`
   so non-login child processes (rage spawning claude) see it on
   the default Debian PATH without depending on shell-init plumbing.

The chained `&&` between `forge-bootstrap` and `forge-ready` is what
makes a failed bootstrap actually fail the env create. Cloud-init
runcmd entries run sequentially regardless of exit status, so without
the chain a botched install would still let the ready signal fire and
`forge env create` would falsely report success.

Version values come from `forge.yaml`:

```yaml
bootstrap:
  k3s: v1.32.0+k3s1
  claude_code: latest
  helm: v3.20.2
```

---

## 9. Network model

The vmnet-NAT mode shipped by Apple Virtualization.framework is not used.
Corporate VPN clients (Cisco AnyConnect's NEPacketTunnelProvider in
particular) intercept macOS traffic at the Network Extension layer below
routing; vmnet-NAT'd VM packets are policy-distinguishable from
Mac-process traffic and get dropped silently when tunnel-all is on. The
VM gets no internet, no DNS, often not even ICMP to the gateway.

FORGE replaces vmnet with a userspace TCP/IP stack on the host
(`gvisor-tap-vsock`) and adds dedicated vsock channels for SSH and
Forgejo. **No host↔VM path depends on the macOS IP routing table**, so
the whole thing works regardless of VPN state.

### Per-env channels

| Channel | Direction | Wire | Used for |
|---|---|---|---|
| `net.sock` (gvproxy) | guest → host (egress) | virtio-net unixgram, `192.168.127.0/24` | apt, curl, k3s pulls, RAGE upstream — every VM `socket()` becomes a host `socket()` call |
| `ssh.sock` | host → guest | virtio-vsock port 22, `connect` mode | `forge env connect` (SSH ProxyCommand) |
| `forgejo.sock` | guest → host | virtio-vsock port 4000, `listen` mode | `git push` from inside the VM to the host's Forgejo |
| `vsock.sock` | guest → host (one-shot) | virtio-vsock port 1234, `listen` mode | First-boot ready signal; Go-bound, only present during `forge env create` |
| `rage-share` | shared dir | virtio-fs, read-only | `~/.forge/rage` exposed into the guest at `/mnt/rage-host` for the rage binary copy |
| `workspace-share` | shared dir | virtio-fs, read-write | `<envDir>/workspace` mounted at `/home/forge/workspace` |

### gvproxy: VM internet

`internal/gvproxy` wraps `github.com/containers/gvisor-tap-vsock`. One
instance per env, started by `forge env _net` (a hidden cobra
subcommand, re-execed by `forge env create`/`start` and detached so it
outlives the parent process — same pattern as vfkit). It binds
`<envDir>/net.sock` (a SOCK_DGRAM unix socket), runs DHCP/DNS/NAT in
userspace, and translates VM packets to host `socket()` calls.

Default subnet: `192.168.127.0/24`. Gateway: `.1`. A magic IP `.254`
NATs to the host's loopback (`127.0.0.1`), so the VM can reach
host-local services via a stable IP. The VM's static IP is allocated
by `internal/env/ipalloc.go` (range `.42–.253`) and written into the
cloud-init `network-config`.

DNS resolution is served by gvproxy's built-in resolver, forwarding to
the host's configured resolvers. `1.1.1.1` is also configured as a
fallback in cloud-init's `network-config`; either path reaches the
internet via gvproxy's NAT layer.

**Lifecycle ordering matters:** gvproxy MUST start before vfkit. vfkit's
virtio-net device dials the unix socket at boot — if nothing is bound
yet, the VM has no NIC. `internal/env/{create,start}.go` enforces
gvproxy → forgejoproxy → vfkit on start, and reverse on stop.

### Forgejo access from VM

The VM dials `localhost:<forgejo-port>` (matching the URL the host clone
recorded in `.git/config`, so no `insteadOf` rewrite is needed). The
in-VM `forge-forgejo-vsock.service` runs
`socat TCP-LISTEN:<port>,bind=127.0.0.1 VSOCK-CONNECT:2:<port>` to
forward the connection over vsock. vfkit's third virtio-vsock device
(in `listen` mode) dials `<envDir>/forgejo.sock`; the host-side
`internal/forgejoproxy` accepts and forwards each connection to
`127.0.0.1:<forgejo-port>` on the host.

This is layered defense alongside gvproxy's `192.168.127.254` NAT —
both paths work. The vsock path never touches the IP stack so it's
guaranteed to keep working through any future netstack issue. The
URL FORGE bakes into `~/.git-credentials` is the localhost form to
match what the host clone recorded.

### IP discovery

The VM's static IP is set in cloud-init's `network-config` and
recorded in `state.json` at create time. The boot-ready vsock signal
also reports it back as a sanity check, but FORGE no longer needs to
"discover" anything — the IP is deterministic.

---

## 10. SSH & access

### Per-env keypair

- Generated at `forge env create`: `ed25519` keypair
- Private key: `~/.forge/envs/<name>/id_ed25519` (mode 0600)
- Public key injected via cloud-init `authorized_keys`
- `forge env connect` uses `-i ~/.forge/envs/<name>/id_ed25519` automatically
- Destroyed with the env

### Optional user key injection

If `ssh.inject_user_key: true` in `~/.forge/config.yaml`, FORGE also adds the user's public key (default: `~/.ssh/id_ed25519.pub`, configurable via `ssh.user_key_path`). Allows direct `ssh` and VS Code Remote SSH without using FORGE CLI.

---

### Managed (default)

`forge system start --mode new` runs Forgejo as a Docker container:

- Image: pinned in `internal/forgejo/pins.go` (tag + sha256 digest, refreshed by `scripts/refresh-pins.sh` under a 14-day soak rule)
- Port: probed from 3000 upward; first free port wins
- Data volume: `~/.forge/forgejo/`
- Admin: user supplies username (default `forge`) and password; FORGE provisions an API token. Token is stored in `~/.forge/config.yaml`; password is the user's to remember (it's the web-UI login)

### External

Connect to an already-running Forgejo (e.g. CAGE's) with `forge system start --mode existing`. FORGE verifies the admin credentials work and provisions its own `forge-cli` API token. The result in `~/.forge/config.yaml`:

```yaml
forgejo:
  url: "http://localhost:4000"
  token: "..."
```

When `forgejo.url` is set, `forge system stop` is a no-op (no managed container to stop). `start`, `status`, and `disconnect` all work in both modes — `disconnect` is config-only (clears the saved connection so a new one can be set up).

### Per-env user and repo

Each `forge env create <name>` provisions a Forgejo user `<name>` and a `workspace` repo under it, then pre-configures the VM's git remote to `<forgejo>/<name>/workspace.git` via cloud-init. Mirrors CAGE so the two tools can share a single Forgejo instance.

`forge env destroy <name>` keeps the Forgejo user and repo by default (review history persists). `--purge-forgejo` deletes the user and all its repos.

---

## 12. Config reference

### `forge.yaml` (project-level, checked into repo)

```yaml
# Component versions installed via cloud-init at forge env create
bootstrap:
  k3s: v1.32.0+k3s1
  claude_code: latest
  helm: v3.20.2

# Resource defaults for this project (override global defaults)
defaults:
  cpus: 2
  memory: 4096   # MB
  disk: 20480    # MB
```

### `~/.forge/config.yaml` (global, machine-specific)

```yaml
forgejo:
  url: ""              # empty = use local managed container
  token: ""            # required if url is set
  port: 3000           # local port (when url is empty)
  admin_user: "forge"  # set on first managed-mode start
  admin_token: "..."   # generated on first managed-mode start

image:
  cache_dir: "~/.forge/images"

ssh:
  inject_user_key: false
  user_key_path: "~/.ssh/id_ed25519.pub"
```

Created on first `forge system start`. Safe to delete to start fresh; `forge system disconnect` clears the `forgejo` block in place.

---

## 13. Host directory layout

```
~/.forge/
├── config.yaml                      # global config
├── images/
│   ├── forge-base-v0.1.0-arm64.img  # cached base images
│   └── forge-base-v0.1.0-arm64.sbom.cdx.json
├── forgejo/                         # Forgejo data volume
├── rage/                            # populated by `forge init` from a project's rage/
│   ├── rage-aarch64-linux           # per-arch rage binary
│   └── rage.toml                    # virtio-fs-shared into every env as `rage-share`
└── envs/
    └── <name>/
        ├── state.json               # name, status, ip, mac, pid, created_at, image_version
        ├── vfkit.pid                # vfkit subprocess
        ├── vfkit.log                # vfkit stdout/stderr
        ├── gvproxy.pid              # forge env _net subprocess (gvisor-tap-vsock)
        ├── gvproxy.log
        ├── forgejo-proxy.pid        # forge env _proxy subprocess (vsock→Forgejo TCP forwarder)
        ├── forgejo-proxy.log
        ├── id_ed25519               # per-env SSH private key (0600)
        ├── id_ed25519.pub           # per-env SSH public key
        ├── disk.img                 # VM disk (copy-on-write from base image)
        ├── cloud-init.iso           # generated cloud-init ISO
        ├── efi-vars                 # vfkit EFI variable store
        ├── net.sock                 # gvproxy unixgram socket; vfkit's virtio-net dials this
        ├── ssh.sock                 # vfkit-bound; SSH ProxyCommand target
        ├── forgejo.sock             # vfkit-bound; in-VM 'git push' rides this via vsock
        ├── vsock.sock               # boot-complete signal channel (only present during create)
        └── workspace/               # `git clone` of the env's Forgejo repo;
            └── …                    # virtio-fs-shared into the VM at /home/forge/workspace
```

---

## 14. Go project structure

```
forge/
├── cmd/
│   ├── root.go              # root cobra command + global flags
│   ├── version.go           # forge version
│   ├── init.go              # forge init — scaffold a forge.yaml
│   ├── env/
│   │   ├── env.go           # env command group
│   │   ├── create.go
│   │   ├── connect.go
│   │   ├── list.go
│   │   ├── start.go
│   │   ├── stop.go
│   │   ├── destroy.go
│   │   ├── net.go           # hidden 'forge env _net' (gvproxy subprocess) + execNetRunner
│   │   └── proxy.go         # hidden 'forge env _proxy' (forgejoproxy subprocess) + execProxyRunner
│   ├── system/
│   │   ├── system.go        # system command group
│   │   ├── start.go         # mode-aware start (existing | new)
│   │   ├── stop.go
│   │   ├── status.go
│   │   ├── disconnect.go    # clear Forgejo block in global config
│   │   └── port.go          # port-probing helper for managed mode
│   └── image/
│       ├── image.go         # image command group
│       ├── pull.go
│       ├── import.go
│       └── list.go
├── internal/
│   ├── env/                 # env lifecycle orchestration (testable, separate from cmd/)
│   │   ├── create.go
│   │   ├── start.go
│   │   ├── stop.go
│   │   ├── destroy.go
│   │   └── disk.go          # base-image → per-env disk copy
│   ├── vm/
│   │   ├── manager.go       # VM lifecycle (start/stop, PID tracking)
│   │   ├── vfkit.go         # vfkit subprocess interface
│   │   ├── mac.go           # MAC address generation
│   │   └── state.go         # state machine + JSON persistence
│   ├── cloudinit/
│   │   ├── userdata.go      # user-data templating (incl. forge user .gitconfig + .git-credentials)
│   │   └── iso.go           # NoCloud ISO generation
│   ├── vsock/
│   │   └── listener.go      # boot-complete signal listener
│   ├── gvproxy/
│   │   └── gvproxy.go       # gvisor-tap-vsock wrapper — VM internet (subnet 192.168.127.0/24)
│   ├── forgejoproxy/
│   │   └── proxy.go         # per-env unix-socket → TCP forwarder so VM-side
│   │                        # 'git push' reaches the host's Forgejo via vsock
│   ├── image/
│   │   ├── source.go        # ImageSource interface
│   │   ├── github.go        # GitHubReleasesSource implementation
│   │   ├── import.go        # local-import path
│   │   └── cache.go         # local image cache management
│   ├── forgejo/
│   │   ├── manager.go       # Forgejo Docker container lifecycle
│   │   ├── api_bootstrap.go # admin verify + API token provisioning
│   │   ├── repos.go         # per-env user + workspace repo management
│   │   └── pins.go          # pinned image tag + sha256 digest
│   ├── ssh/
│   │   └── keygen.go        # per-env ed25519 keypair generation
│   ├── progress/
│   │   └── progress.go      # Step-based progress UI: spinner on TTY, plain lines otherwise
│   └── config/
│       ├── global.go        # ~/.forge/config.yaml loader
│       └── project.go       # forge.yaml loader
├── images/base/             # base VM image build pipeline
├── scripts/
│   └── refresh-pins.sh      # refreshes pinned image digests (14-day soak)
├── docs/spec.md             # this file
├── .github/workflows/       # CI: test, integration, image release
├── go.mod
├── go.sum
├── Makefile
└── forge.yaml               # this project's own forge.yaml
```

Dependencies (following CAGE's stack):
- `github.com/spf13/cobra` — CLI framework
- `github.com/rs/zerolog` — structured logging
- `github.com/charmbracelet/lipgloss` — terminal UI
- `golang.org/x/crypto` — SSH key generation
- `gopkg.in/yaml.v3` — config parsing
- `github.com/containers/gvisor-tap-vsock` — userspace TCP/IP stack (gvproxy). Pulls in `gvisor.dev/gvisor` as a transitive dependency. Requires Go 1.24+.

---

## 15. Security model

| Layer | Mechanism |
|-------|-----------|
| Kernel isolation | Apple Virtualization.framework hypervisor boundary |
| Network isolation | gvproxy userspace netstack: VM has no L2 access to the host network and cannot receive inbound connections from outside the host. Outbound from the VM appears as Mac-process traffic via host `socket()` calls, so corp VPN policies apply uniformly. |
| API filtering | RAGE proxy inside VM intercepts Claude Code's Anthropic API calls |
| Git review gate | Forgejo — agent pushes to local Forgejo; human reviews before production |
| SSH access | Per-env keys; destroyed with the env |
| No root | VM user `forge` has sudo but Claude Code/RAGE run as `forge` |
| SBOM | Published with every base image release; scannable by Grype/Trivy |

---

## 16. Build & test plan

### Development loop

```bash
go build -o forge .                    # produce ./forge
go test ./...                          # unit tests
golangci-lint run ./...                # lint
./images/base/build-in-docker.sh       # build base VM image locally (Docker-wrapped libguestfs)
./scripts/refresh-pins.sh              # refresh pinned image digests (14-day soak)
```

`Makefile` provides `build`, `test`, `lint`, `clean` as thin wrappers. `pre-commit install` wires the same checks into git commits.

### Test strategy (TDD — tests first)

- Unit tests for: state machine transitions, cloud-init generation, config parsing, ImageSource interface
- Integration tests (require vfkit + macOS): VM create/connect/destroy happy path, vsock signalling, SSH key injection
- No mocking of vfkit — integration tests hit the real binary

### CI (GitHub Actions)

- `test`: unit tests on every push (Linux runner, unit tests only)
- `integration`: integration tests on macOS runner (self-hosted Apple Silicon)
- `image`: build + publish base image on tag push; attach SBOM to GitHub Release

---

## 17. Migration path to OCI registry

When the team's central OCI registry is ready:

1. Implement `OCIRegistrySource` satisfying the `ImageSource` interface
2. Set `image.source: oci` and `image.registry: registry.example.com` in `~/.forge/config.yaml`
3. Update CI to push to OCI instead of GitHub Releases (or both)
4. SBOM attaches via OCI referrers API (`cosign attach sbom` / `oras attach`)

No changes to the rest of FORGE. The interface boundary is the entire migration surface.
