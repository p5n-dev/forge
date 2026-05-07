# FORGE

**F**ederated **O**rchestrated **R**untime **G**uarded **E**nvironment

VM-based sandbox for running AI coding agents (Claude Code) in isolation, with native Kubernetes support.

---

## What is FORGE?

FORGE creates ephemeral, hypervisor-isolated VMs for AI coding sessions. Each environment boots a Debian VM with k3s, RAGE, and Claude Code installed, gives the developer SSH access, and uses a local Forgejo instance as a Git review gate before any code lands in production.

It's the VM-based sibling of two existing tools:

| Tool | Isolation | Purpose |
|------|-----------|---------|
| **[CAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/cage)** | Docker container | General coding tasks |
| **FORGE** (this project) | Apple Virtualization.framework VM | Tasks that need real Kubernetes |
| **[RAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/rage)** | API-level proxy | Runtime guardrails inside CAGE/FORGE |

## Why FORGE exists

CAGE works well for the majority of AI coding work, but Kubernetes does not run properly inside a Docker container — k3s and kubeadm both require privileged containers, nested namespaces, and a shared kernel, which undermine the isolation model.

FORGE solves this by replacing the container with a virtual machine:

- **True kernel isolation** via the Apple Virtualization.framework hypervisor boundary
- **Native Kubernetes** with k3s running directly on the guest's Linux kernel
- **Same developer experience** as CAGE — CLI-driven, ephemeral, with Forgejo as the review gate

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Host (macOS Apple Silicon)                                      │
│                                                                  │
│  ┌─────────────┐                                                 │
│  │  Forgejo    │──┐                                              │
│  │  (Docker)   │  │ vsock + unix socket                          │
│  │             │  │ (forgejoproxy, VPN-immune)                   │
│  │  Git review │  ▼                                              │
│  │  gate       │ ┌──────────────────────────────────┐            │
│  └─────────────┘ │  VM (Debian arm64)               │            │
│                  │                                  │            │
│  ┌─────────────┐ │  ┌────────────────────────────┐  │            │
│  │  FORGE CLI  │ │  │  k3s (single-node)         │  │            │
│  │  (Go/cobra) │ │  │  ┌──────────────────────┐  │  │            │
│  └─────────────┘ │  │  │  Workloads           │  │  │            │
│         │        │  │  └──────────────────────┘  │  │            │
│         │ vsock  │  └────────────────────────────┘  │            │
│         └───────►│                                  │            │
│         (SSH)    │  RAGE → Claude Code              │            │
│                  │  openssh-server                  │            │
│                  │  cloud-init (bootstrap)          │            │
│                  └─────────────┬────────────────────┘            │
│                                │ virtio-net via unix socket      │
│                                ▼                                 │
│                  ┌──────────────────────────────────┐            │
│                  │  gvproxy (userspace netstack)    │            │
│                  │  192.168.127.0/24, DHCP, DNS,    │            │
│                  │  per-conn host socket() calls    │            │
│                  └─────────────┬────────────────────┘            │
│                                │                                 │
│                                ▼ macOS network stack             │
│                            (internet, with corp VPN if any)      │
└──────────────────────────────────────────────────────────────────┘
```

Three independent host↔guest channels per env: SSH (vsock), Forgejo (vsock),
and general egress (gvproxy / userspace TCP/IP). None of them traverse the
host's IP routing table to the VM, so all three work with a tunnel-all corp
VPN connected. See `CLAUDE.md` § Networking model for the full picture.

The full architecture spec lives in [`docs/spec.md`](./docs/spec.md).

## Status

MVP complete. macOS Apple Silicon only. Linux/KVM, OCI registry distribution, and multi-node clusters are tracked as post-MVP work.

## Requirements

Day-to-day usage needs two host-side tools:

| Tool | Install | Why |
|------|---------|-----|
| `vfkit` | `brew install vfkit` | Apple Virtualization.framework wrapper |
| Docker | `brew install --cask docker` | Hosts the local Forgejo container |

`forge` itself is distributed as a pre-built single binary on the [GitHub Releases page](https://github.com/p5n-dev/forge/releases) — see **Quick Start** below. If you want to build from source or hack on FORGE itself, the **Contributing** section near the end has the full local-dev setup.

## Quick start

### 1. Install `forge`

Pre-built binaries are published to [GitHub Releases](https://github.com/p5n-dev/forge/releases) on every code-touching push to `main` (markdown-only edits don't trigger a release). Apple Silicon is the supported runtime.

```sh
# Pick the latest release tag from the Releases page (e.g. v2026.05.06.42-gabc1234).
TAG=v2026.05.06.42-gabc1234
curl -L -o forge \
    "https://github.com/p5n-dev/forge/releases/download/${TAG}/forge-darwin-arm64"
chmod +x forge
sudo install -m 0755 forge /usr/local/bin/forge

# Verify integrity against the release's SHA256SUMS
curl -L "https://github.com/p5n-dev/forge/releases/download/${TAG}/SHA256SUMS" \
    | shasum -a 256 -c -

forge version    # forge v2026.05.06.42-gabc1234
```

To build from source instead, see **Contributing**.

### 2. Get a base VM image into the cache

```sh
forge image pull
```

If no GitHub Release exists yet, see [Contributing → Base image: building locally](#base-image-building-locally) for how to build and import one yourself.

### 3. Wire up Forgejo

FORGE needs a Forgejo instance to use as the Git review gate. Just run:

```sh
forge system start
```

It will ask whether to use an existing Forgejo or spin up a fresh one:

```
How would you like to set up Forgejo?
  [1] Use an existing Forgejo instance (e.g. one CAGE is already running)
  [2] Start a new FORGE-managed Forgejo container
Choose [1/2]:
```

**Option 1 — Use an existing Forgejo** (e.g. CAGE's, or a team-shared instance)

You'll be asked for the URL (defaults to `http://localhost:4000`), the admin username, and the admin password. FORGE verifies the credentials work and that the user has admin scope, then provisions a fresh API token named `forge-cli` and writes URL + token to `~/.forge/config.yaml`. No Docker container is started.

**Option 2 — Start a new FORGE-managed Forgejo**

FORGE probes from port 3000 upward, picks the first free port, then asks for admin username (defaults to `forge`) and password. It creates the container, the admin user, and an API token, then persists everything to `~/.forge/config.yaml`. **The password is yours to remember** — it's how you log into the Forgejo web UI; FORGE only ever stores the API token.

**Non-interactive (CI / scripts)**

```sh
# existing Forgejo
FORGE_ADMIN_PASSWORD='...' forge system start --mode existing \
    --forgejo-url http://localhost:4000 --admin-user admin

# new Forgejo
FORGE_ADMIN_PASSWORD='...' forge system start --mode new --admin-user forge
```

### 4. Drop a RAGE binary in your project (optional but recommended)

If you want every env to launch with RAGE wrapping Claude Code (the default `forge env connect` path), place the Linux RAGE binary and `rage.toml` next to your `forge.yaml`:

```sh
cd ~/code/myproject
mkdir -p rage
# Apple Silicon hosts → arm64 VMs → arm64 binary
curl -L -o rage/rage-aarch64-linux \
    https://sbp.gitlab.schubergphilis.com/sbp-ai/rage/-/releases/permalink/latest/downloads/binaries/rage-aarch64-linux
chmod +x rage/rage-aarch64-linux
cp /path/to/your/rage.toml rage/
```

The filename **must** be exactly `rage-aarch64-linux` (or `rage-x86_64-linux` for x86_64 VMs) — that's the CAGE convention FORGE looks for. The next `forge init` will copy the directory into `~/.forge/rage`, and every env you create gets RAGE installed via the `rage-share` virtio-fs mount.

If you skip this step, the env still boots but `forge env connect` (without `--bash`) will fail with "rage: command not found" — `--bash` works regardless.

### 5. Create, connect, destroy

```sh
cd ~/code/myproject
forge init                 # optional — drops a forge.yaml + copies rage/ into ~/.forge/rage
forge env create myproj
forge env connect myproj   # → Claude Code session via RAGE, cwd = /home/forge/workspace
forge env destroy myproj
```

`forge env create` shows a per-step spinner so you can see where time is being spent (disk prep, Forgejo provisioning, VM boot, in-VM bootstrap). The bootstrap phase — pinning + verifying the k3s, helm, and claude-code installer scripts, copying RAGE in from the virtio-fs share, downloading the native claude binary — is the slowest and can take 2–5 minutes on a fresh image. When stdout is redirected (CI, log files), the spinner is replaced with one line per step transition for log-friendly output.

`forge init` is optional. `forge env create` walks up from the current directory looking for a `forge.yaml`. If it doesn't find one and you're running interactively, it prompts you to initialise the current directory before continuing — answer `n` to use built-in defaults instead. In non-interactive runs (CI / piped stdin) the prompt is skipped: pass `--no-init` to opt into built-in defaults explicitly, or run `forge init` ahead of time.

### 6. Useful while it's running

```sh
forge env list                      # see all envs and their status
forge env logs myproj -f            # tail bootstrap log (works mid-create)
forge env connect myproj --bash     # interactive shell instead of Claude Code
forge doctor                        # vfkit / gvproxy / per-env reachability check
```

Networking note: it all works with a tunnel-all corp VPN connected. SSH rides a vsock-bridged Unix socket; in-VM `git push` rides another; general internet egress goes through a userspace TCP/IP stack on the host (`gvproxy`). No path through the host's IP routing table, so the VPN's NEPacketTunnelProvider can't intercept anything. See `CLAUDE.md` § Networking model for the wire-level details.

### 7. Cleanup (managed-mode Forgejo only)

```sh
forge system stop
```

Skip this step if you chose **option 1 (use an existing Forgejo)** — FORGE never started a container, so there's nothing to stop. `forge system disconnect` clears the saved connection in either mode.

## CLI reference

### Project setup

| Command | Description |
|---------|-------------|
| `forge init [path]` | Write a default `forge.yaml` into the given directory (or CWD), AND copy a project-local `rage/` dir into `~/.forge/rage`. `--force` overwrites both. |
| `forge doctor` | Health-check vfkit, per-env reachability over `ssh.sock`, and per-env `net.sock` (gvproxy). Exits non-zero on any FAIL. |

`forge init` is optional for projects that don't use rage. `forge env create` and `forge env start` look for a `forge.yaml` in the current directory and walk upward through ancestor directories. When `forge env create` finds none and is running interactively, it prompts to initialise the current directory; pass `--no-init` (or answer `n` at the prompt) to use the same built-in defaults that `init` writes. `forge env start` always uses defaults silently — by then an env already exists, so the prompt would be too late.

If you place the rage Linux binary (`rage-aarch64-linux` or `rage-x86_64-linux`) and `rage.toml` in a `rage/` subdirectory next to `forge.yaml` (the [CAGE convention](docs/cage-README.md)), `forge init` copies them into `~/.forge/rage`. From there every env you create gets RAGE wired up automatically via the `rage-share` virtio-fs mount.

### Environment lifecycle

| Command | Description |
|---------|-------------|
| `forge env create [name]` | Boot a new VM, run cloud-init bootstrap, clone the Forgejo repo into the env's workspace, wait for the boot-ready vsock signal |
| `forge env connect [name]` | SSH into the VM (via the vsock-bridged Unix socket) and launch RAGE → Claude Code, with cwd at `/home/forge/workspace` (the env's Forgejo project). Works during `starting` too — useful for `tail -f /var/log/forge-bootstrap.log` while the VM is still bootstrapping. |
| `forge env connect [name] --bash` | Same path, drops to an interactive shell in `/home/forge/workspace`. Login profile is sourced so `kubectl`, `claude`, `rage`, and `KUBECONFIG` all Just Work. |
| `forge env list` | Show all envs with live status (running / stopped / crashed / starting) |
| `forge env start [name]` | Restart a stopped or crashed env. Waits on the SSH banner over `ssh.sock` (warm boot, ~10–15 s on M-series). |
| `forge env stop [name]` | Gracefully shut down a running env. `--force` to clear an env stuck in starting/stopping/crashed. |
| `forge env destroy [name]` | Stop and delete the env entirely (disk, SSH keys, cloud-init, **workspace**, state). |

`forge env create` accepts `--cpus`, `--memory`, `--disk`, and `--image` flags to override the defaults from `forge.yaml`.

**Connection path:** `forge env connect` does NOT touch the macOS routing table. SSH rides a vsock-bridged Unix socket end-to-end (host-unix → vfkit → guest-vsock → in-VM socat → sshd), so a corporate VPN that hijacks `192.168.x.0/24` ranges can't intercept the connection. The same vsock-bridge pattern handles in-VM `git push` to the host's Forgejo (`internal/forgejoproxy`), and a userspace TCP/IP stack on the host (`internal/gvproxy`, based on `gvisor-tap-vsock`) gives the VM general internet access — every VM connection becomes a host-side `socket()` call, which the VPN treats the same as a Mac browser. See `CLAUDE.md` § Networking model for the full picture.

`forge env destroy` deletes the local env (disk, SSH keys, cloud-init, state, **workspace and any uncommitted changes in it**). Forgejo state — the per-env user and its `workspace` repo — is **kept by default** so review history sticks around. Pass `--purge-forgejo` to also delete the Forgejo user (and all repos under it).

### System (Forgejo)

| Command | Description |
|---------|-------------|
| `forge system start` | Set up the Forgejo connection (existing instance or fresh managed container) |
| `forge system stop` | Stop the FORGE-managed Forgejo Docker container |
| `forge system status` | Health check: Forgejo reachability, vfkit, image cache |
| `forge system disconnect` | Forget the saved Forgejo connection in `~/.forge/config.yaml` |

`start` works in both modes; `stop` only does anything when FORGE is managing its own container. `status` and `disconnect` work in either mode — `disconnect` is config-only, useful for rotating the API token or pointing FORGE at a different Forgejo. If you want to fully tear down a managed Forgejo, run `stop` first and then `disconnect`.

### Image management

| Command | Description |
|---------|-------------|
| `forge image pull [version]` | Download base image + SBOMs from GitHub Releases |
| `forge image import <path>` | Copy a locally-built or otherwise-obtained image into the cache |
| `forge image list` | List locally cached images |

`forge image import` is the entry point for local development — see [Contributing → Base image: building locally](#base-image-building-locally) below.

## Configuration

FORGE has two layers of config: project-level (in the repo) and global (per-machine).

### `forge.yaml` — project-level

Lives at the root of your project. Pins the bootstrap component versions and per-project resource defaults. Should be checked into the repo.

```yaml
bootstrap:
  k3s: v1.32.0+k3s1
  rage: v0.4.2
  claude_code: latest
  helm: v3.20.2

defaults:
  cpus: 2
  memory: 4096   # MB
  disk: 20480    # MB
```

Create one with `forge init`. `forge env create` and `forge env start` find it by walking up from the current directory, so it works from any subdirectory of your project. With no `forge.yaml` anywhere up to the filesystem root, FORGE falls back to built-in defaults equivalent to a freshly-`init`-ed file.

### `~/.forge/config.yaml` — global

Personal, per-machine settings. Created on first `forge system start`; safe to delete to start fresh.

```yaml
forgejo:
  url: ""              # empty → use local managed container
  token: ""            # required if url is set
  port: 3000           # local port (when url is empty)
  admin_user: "forge"  # auto-generated on first start
  admin_token: "..."   # auto-generated on first start

image:
  cache_dir: "~/.forge/images"

ssh:
  inject_user_key: false
  user_key_path: "~/.ssh/id_ed25519.pub"
```

## Forgejo integration

Each `forge env create <name>` does the following against Forgejo:

1. Creates a user named `<name>` with email `<name>@forge.local` (idempotent — re-create is fine).
2. Creates a `workspace` repository under that user.
3. Returns the clone URL `<forgejo>/<name>/workspace.git` and pre-configures it as `origin` inside the VM via cloud-init.

This mirrors CAGE's pattern, so the two tools can share the same Forgejo instance and produce a consistent `<env>/workspace` URL layout regardless of which tool created the env.

`forge env destroy <name>` only removes the local env by default. Pass `--purge-forgejo` to also delete the Forgejo user and all of its repos. Without that flag, the review history stays put — useful when you want to spin the env back up later or keep an audit trail after the VM is gone.

## On-disk layout

Everything FORGE owns lives under `~/.forge/`:

```
~/.forge/
├── config.yaml                      # global config
├── images/
│   └── forge-base-<ver>-arm64.img.gz
├── forgejo/                         # Forgejo Docker volume
├── rage/                            # populated by `forge init` from a project's rage/ dir
│   ├── rage-aarch64-linux           # or rage-x86_64-linux, etc.
│   └── rage.toml                    # virtio-fs-shared into every env as `rage-share`
└── envs/
    └── <name>/
        ├── state.json               # name, status, ip, mac, pid, …
        ├── vfkit.pid                # vfkit subprocess
        ├── vfkit.log
        ├── gvproxy.pid              # userspace netstack subprocess (forge env _net)
        ├── gvproxy.log
        ├── forgejo-proxy.pid        # vsock→TCP forwarder for Forgejo (forge env _proxy)
        ├── forgejo-proxy.log
        ├── id_ed25519               # per-env SSH key (0600)
        ├── id_ed25519.pub
        ├── disk.img
        ├── cloud-init.iso
        ├── efi-vars
        ├── net.sock                 # gvproxy unixgram socket; vfkit's virtio-net dials this
        ├── ssh.sock                 # vfkit-bound; SSH ProxyCommand target
        ├── forgejo.sock             # vfkit-bound; in-VM 'git push' rides this via vsock
        ├── vsock.sock               # boot-complete signal (Go-bound; only present during create)
        └── workspace/               # `git clone` of the env's Forgejo repo;
            └── …                    # virtio-fs-shared into the VM at /home/forge/workspace
```

Destroying an env removes the entire `<name>/` directory — **including any uncommitted changes in `workspace/`.**

## Project layout

```
forge/
├── cmd/                  Cobra CLI command implementations
│   ├── root.go
│   ├── version.go
│   ├── doctor.go         forge doctor — health probe
│   ├── env/              forge env create / connect / list / start / stop / destroy
│   │                      + hidden _net (gvproxy) and _proxy (forgejoproxy) subcommands
│   ├── system/           forge system start / stop / status / disconnect
│   └── image/            forge image pull / list
├── internal/
│   ├── config/           Two-level config loader (forge.yaml + global)
│   ├── env/              Orchestration for env lifecycle (testable)
│   ├── vm/               vfkit subprocess wrapper, state machine, MAC gen
│   ├── gvproxy/          Userspace TCP/IP stack on a unix socket per env
│   │                      (gvisor-tap-vsock wrapper). VM internet access.
│   ├── forgejoproxy/     Per-env unix-socket → TCP forwarder so VM-side
│   │                      'git push' reaches the host's Forgejo via vsock.
│   ├── image/            ImageSource interface + GitHubReleasesSource
│   ├── forgejo/          Docker-backed Forgejo lifecycle + REST client
│   ├── ssh/              Per-env ed25519 keypair generation
│   ├── cloudinit/        user-data templating + NoCloud ISO generation
│   └── vsock/            Unix-socket listener for guest boot signal
├── images/base/          Build pipeline for the thin base VM image
├── docs/spec.md          Architecture specification
├── .github/workflows/    CI: test, integration, image release
├── forge.yaml            This project's own forge.yaml
└── Makefile
```

## Contributing

### Tools

In addition to the day-to-day `vfkit` + Docker:

| Tool | Install | Why |
|------|---------|-----|
| Go 1.24+ | `brew install go` | Build `forge` from source. |
| `golangci-lint` | `brew install golangci-lint` | Lint check; CI runs the same. |
| `goimports` | `go install golang.org/x/tools/cmd/goimports@latest` | Import ordering — run by the pre-commit hook. |
| `pre-commit` | `brew install pre-commit && pre-commit install` | Runs `gofmt` / `goimports` / lint / build / `go test -short` on every commit. Optional but recommended; CI runs the same checks. |

### Build + test

```sh
git clone https://github.com/p5n-dev/forge.git
cd forge
go build -o forge .       # produces ./forge in CWD
sudo install -m 0755 forge /usr/local/bin/forge   # or just leave in CWD
go test -short ./...      # fast unit suite (no external deps)
golangci-lint run ./...   # full lint
```

The unit suite has no external dependencies — every collaborator (vfkit, Docker, Forgejo, vsock, gvproxy) is abstracted behind an interface and faked in tests, so `go test -short ./...` runs in seconds. The longer-running integration tests live in `.github/workflows/integration.yml` and run on a self-hosted macOS Apple Silicon runner against a real vfkit; they exercise the full `create → ssh probe → stop → start → destroy` lifecycle.

### Release workflow

Three GitHub Actions workflows drive CI/CD:

| Workflow | Trigger | What it does |
|----------|---------|--------------|
| `.github/workflows/test.yml` | every push + PR | Build + unit tests + golangci-lint on every branch. |
| `.github/workflows/release.yml` | push to `main` (code-only) | Cross-compiles `forge-darwin-arm64` and `forge-darwin-amd64`, computes `SHA256SUMS`, attaches all three to a new GitHub Release auto-named `v$(date).$(run-number)-g$(short-sha)`. **Pure markdown / docs / CI-config edits are excluded via `paths-ignore`** — they don't trigger a release. |
| `.github/workflows/image.yml` | push of a `v*` tag | Builds the base VM image (`.img.gz`), generates SPDX + CycloneDX SBOMs with `syft`, attaches the artefacts to the same release as the binary. |

So the day-to-day flow is:

- Open a PR → `test.yml` validates the change.
- Merge to `main` → if the diff touched any code, `release.yml` cuts a new versioned binary release on the [Releases page](https://github.com/p5n-dev/forge/releases). If the diff was docs only, nothing happens.
- For an explicit base-image release, push a `v*` tag → `image.yml` produces and uploads the `.img.gz` + SBOMs.

### Supply-chain hygiene

Every external dependency is pinned by version AND content hash. The constants live in source (`internal/forgejo/pins.go`, `internal/cloudinit/userdata.go`, `images/base/Dockerfile`, `images/base/build.sh`, `.github/workflows/image.yml`) with `pin:<name>-…` sentinel comments next to the values; `scripts/refresh-pins.sh` re-derives them from the upstream releases under a 14-day soak rule. Don't hand-edit pinned constants — re-run the script. See `CLAUDE.md` § "Supply-chain hardening" for the full rules.

### Base image: building locally

The base VM image is published as a GitHub Release artefact, but you may want to iterate on it locally without going through CI.

On Linux:

```sh
cd images/base
VERSION=dev ./build.sh
forge image import output/forge-base-dev-arm64.img.gz
```

On macOS (no host libguestfs needed — the Docker wrapper handles it):

```sh
cd images/base
VERSION=dev ./build-in-docker.sh
forge image import output/forge-base-dev-arm64.img.gz
```

Both paths produce identical artefacts. See [`images/base/README.md`](./images/base/README.md) for the full env-var matrix (Debian release, arch, output paths).

The thin base image is a Debian arm64 minimal install (Debian 13 / trixie by default; configurable via `DEBIAN_VERSION`) with `openssh-server`, `cloud-init`, `curl`, `git`, `ca-certificates`, `sudo`, `socat`, `jq`, the virtio-vsock kernel modules, and a `forge-ready` systemd service. Everything else (k3s, RAGE, Claude Code, helm) is installed at `forge env create` time via cloud-init at versions pinned in the project's `forge.yaml`. This keeps the image's SBOM scope narrow and lets users swap component versions without rebuilding.

### Where to look first

If you're new to the codebase, skim these in order:

1. [`CLAUDE.md`](./CLAUDE.md) — project primer; what's currently true, recent focus areas, and the things easy to get wrong.
2. [`docs/spec.md`](./docs/spec.md) — architecture spec, decision log, package layout, security model.

Both are kept current with each change.

## Security model

| Layer | Mechanism |
|-------|-----------|
| Kernel isolation | Apple Virtualization.framework hypervisor boundary |
| Network isolation | gvproxy userspace netstack: VM has no L2 access to the host network and cannot receive inbound connections from outside the host. Outbound from the VM appears as Mac-process traffic (host `socket()` calls), so corp VPN policies apply uniformly. |
| API filtering | RAGE proxy inside the VM intercepts Claude Code's Anthropic API traffic |
| Git review gate | Forgejo — agent pushes there; human reviews before production |
| SSH access | Per-env ed25519 keypair; destroyed with the env |
| Privilege | VM user `forge` has sudo, but RAGE/Claude Code run as that user, not root |
| SBOM | Published with every base image release; scannable by Grype/Trivy |

## Philosophy

Inherited from CAGE:

- **We trust developers.** The tool protects them FROM bad actors, not from themselves.
- **Don't be a blocker.** Security should be invisible in default mode.
- **Hardened mode is opt-in friction.**
- **Test-driven development.** Tests first, code second.

## Related projects

- [CAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/cage) — Container-based sandbox for general coding tasks
- [RAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/rage) — Runtime API guardrails (used inside CAGE and FORGE)
