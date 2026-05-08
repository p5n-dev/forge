# FORGE — primer for Claude Code agents

This file is the starting point when you join a session on FORGE. Read
it end-to-end before touching code; everything below is condensed.

## What FORGE is

A VM-based sandbox for running AI coding agents (Claude Code) in
isolation, with native Kubernetes (k3s + Helm). Each `forge env`
boots an Apple Virtualization.framework VM via `vfkit`, bootstraps it
with cloud-init, and exposes it through a controlled set of
host↔guest channels — none of which traverse the macOS host's IP
routing table, so VMs work with a tunnel-all corporate VPN connected.

It is the VM-based sibling of two separate projects:

- **CAGE** = Contained Agentic Guarded Environment (Docker-based)
- **RAGE** = Runtime Agent Guardrails Emulator (API-level proxy)
- **FORGE** = Federated Orchestrated Runtime Guarded Environment (VM)

CAGE and RAGE are **not in this repository**. FORGE inherits CAGE's
CLI shape and consumes RAGE as an operator-supplied binary delivered
via a virtio-fs share.

## Status

MVP complete. macOS Apple Silicon only. Linux/KVM, OCI registry
distribution, and multi-node k3s clusters are tracked as post-MVP.

## Read these first

In this order — they describe what's currently true, not what was once
planned:

1. [`README.md`](./README.md) — user-facing entry point, full CLI reference, on-disk layout
2. [`docs/spec.md`](./docs/spec.md) — architecture spec, decision log, security model
3. `forge.yaml` (project-level) and `~/.forge/config.yaml` (global) examples in the README

This file (`CLAUDE.md`) covers **how to work on FORGE** — conventions,
supply-chain rules, package layout, footguns. The three references
above cover **what FORGE does**.

## How we work on this codebase

- **Test-driven.** Tests first, code second. Pre-commit hooks run
  gofmt, goimports, golangci-lint, build, and `go test -short` on
  every commit. CI runs the same plus an integration suite on a
  self-hosted Apple Silicon runner.
- **Terse code.** Short functions. No premature abstractions. No
  half-finished implementations.
- **Comments explain WHY, not WHAT.** Default to no comments. When
  you do leave one, it should explain a decision, a constraint, or a
  past incident — not narrate the code.
- **Commit + push after each logical change.** Don't batch unrelated
  changes into a mega-commit. Clean messages explaining the why; the
  user will pull and review per-commit.
- **Shell scripts must be portable** across BSD (macOS host) and GNU
  (Linux CI / build container). `[[:space:]]` not `\s`; watch for
  `date -d` vs `date -j -f`; BSD `sed -i` needs an empty `''` argument.
- **Supply-chain rules are SACRED** — see the next section.

## Supply-chain hardening — HARD REQUIREMENT

The user is *strict* on supply-chain attack prevention. These rules
are non-negotiable for every change, every PR, every dependency,
forever:

1. **Pin every external dependency by version AND content hash.**
   Versions alone are not enough — most registries allow tag mutation.
   - Container images: `image:tag@sha256:…` (see `internal/forgejo/pins.go`, `images/base/Dockerfile`).
   - Standalone binaries fetched at runtime: pin both the version and a `sha256` constant; the script that fetches them runs `sha256sum -c` before exec.
   - Vendor installer scripts (the `curl | sh` family): pin the **script content's** sha256. The version of the underlying tool is then chosen via a TARGET arg or env var, and the script's own checksum chain verifies the binary it downloads. Two layers of trust: our pin protects the wrapper; the wrapper protects the artefact.
2. **14-day soak before adopting a new version.** `SOAK_DAYS=14` is
   the default in `scripts/refresh-pins.sh` — only pin releases
   published at least that long ago.
3. **`scripts/refresh-pins.sh` is the only legitimate way to change
   a pin.** Don't hand-edit pinned constants. The script enforces
   the soak rule and recomputes the hash from the actual artefact.
   When you add a new pinned dependency, extend the script to refresh
   it — otherwise the pin will rot silently.
4. **Sentinel comments matter.** Every pinned constant has a
   `// pin:<name>-version` or `// pin:<name>-sha256` trailing comment.
   The refresh script uses these as sed anchors. Keep them.
5. **`curl … | sh -` is a smell — only tolerated as a last resort.**
   Pin the script's content too (`get.k3s.io`, `claude.ai/install.sh`,
   `helm/helm/scripts/get-helm-3`, anchore/syft `install.sh`).
6. **`apt-get install` is OK without our own pin** — but only because
   the base image is digest-pinned and apt verifies every `.deb` against
   a GPG-signed Release file. Existing apt usage is in the **build
   pipeline** (Dockerfile, image.yml workflow) — never at VM runtime.
   **Don't introduce `apt-get install` into the in-VM bootstrap path.**

When you add a new dependency: (a) add a placeholder pin with sentinel
comments → (b) extend `scripts/refresh-pins.sh` to populate it →
(c) run the script → (d) commit the populated values together with
the dependency change.

## Where things live

```
cmd/                       Cobra command tree (root.go, doctor.go, version.go, init.go)
├── env/                   forge env <create|connect|list|start|stop|destroy|logs>
│                          + hidden _net / _proxy subcommands (subprocess re-execs)
├── system/                forge system <start|stop|status|disconnect>  (Forgejo lifecycle)
└── image/                 forge image <pull|list|import>

internal/
├── env/                   Env lifecycle orchestrator. create.go, start.go, stop.go,
│                          destroy.go, wait.go (SSH-banner readiness probe), ipalloc.go,
│                          paths.go, disk.go.
├── vm/                    vfkit subprocess + state machine. vfkit.go (CLI builder),
│                          manager.go (PID lifecycle + status), state.go (stopped →
│                          starting → running → stopping/crashed), mac.go.
├── cloudinit/             In-VM bootstrap. userdata.go (the rendered cloud-init
│                          template, including forge-bootstrap script as a heredoc),
│                          iso.go (NoCloud cidata ISO writer), files/statusline.sh
│                          (go:embed-ed Catppuccin statusline for the in-VM claude).
├── forgejo/               Forgejo HTTP API client + container management. pins.go has
│                          the digest-pinned image reference.
├── forgejoproxy/          Host-side unix-socket → TCP forwarder. Subprocess that
│                          bridges the VM's vsock to the host's Forgejo container.
├── gvproxy/               Wrapper around containers/gvisor-tap-vsock. The userspace
│                          TCP/IP stack that gives the VM general egress without
│                          using vmnet.
├── image/                 Base-image source abstraction. github.go fetches from
│                          GitHub Releases (DefaultGitHubOwner is wired up here).
├── config/                forge.yaml (project) + ~/.forge/config.yaml (global)
│                          loaders. project.go has the embedded default forge.yaml
│                          template used when no config exists upstream.
├── ssh/                   Per-env ed25519 keypair generation.
├── vsock/                 Boot-ready vsock listener (port 1234, first-boot-only).
├── progress/              Terminal progress UI (lipgloss).
└── testutil/              Test helpers. SockTempDir(t) for AF_UNIX socket paths
                           on macOS (sun_path is 104 bytes there, vs 108 on Linux —
                           t.TempDir() in /var/folders/... overruns the limit).

images/base/               Base image build pipeline.
├── Dockerfile             Debian-based libguestfs builder image.
├── build.sh               The actual build (downloads Debian cloud qcow2,
│                          virt-customize installs apt deps, registers forge-ready,
│                          generates SBOMs).
├── build-in-docker.sh     Wraps build.sh in the Dockerfile builder. Same path
│                          local devs and CI both use.
└── files/                 forge-ready.service, forge-ready.sh — baked into the image.

scripts/refresh-pins.sh    The ONLY tool that changes pinned-dependency constants.
                           14-day soak rule enforced here.

.github/workflows/         test.yml (lint+test on push/PR), integration.yml (full VM
                           lifecycle on the self-hosted Mac runner), release.yml
                           (cross-compile darwin-arm64+amd64 binaries on v* tag),
                           image.yml (build base image + SBOMs on the same v* tag).
```

## Architecture summary

(See `docs/spec.md` § 9 for wire-level details; this is the 30-second view.)

**Four host↔guest channels per env**, none of which traverse the host
IP routing table:

| Channel | Backing socket | Purpose |
|---|---|---|
| `net.sock` | virtio-net via gvproxy unixgram | VM's general outbound (apt, k3s, claude.ai, …) |
| `ssh.sock` | virtio-vsock port 22 | host → VM SSH via `ProxyCommand="nc -U …"` |
| `forgejo.sock` | virtio-vsock port 4000 | VM → host Forgejo (git push from inside the VM) |
| `vsock.sock` (port 1234) | virtio-vsock | first-boot-only "ready" signal from VM to `forge env create` |

**Two virtio-fs shares per env**:

- `rage-share` ← `~/.forge/rage` (read-only) — the operator-supplied RAGE binary + `rage.toml`. Cloud-init copies them into the VM at boot.
- `workspace-share` ← `<envDir>/workspace` (read-write) — mounted at `/home/forge/workspace` via fstab. The env's Forgejo repo is cloned here at create time; host and the in-VM agent edit the same tree. HostUID-matched (`os.Getuid()`) for ownership parity.

Lifecycle ordering: `gvproxy` MUST start before `vfkit` (vfkit dials
`net.sock` at boot — if nothing's listening, the VM has no NIC).
Then `forgejoproxy`, then `vfkit`. Reverse on stop.

## Sibling projects (context only — not in this repo)

These live on the operator's machine; not visible from inside this
container. Ask the user to fetch specific files by name if you need
them:

- **CAGE** — Go (cobra/zerolog/lipgloss). FORGE inherits its CLI shape, the rage-share convention, and the per-env Forgejo user/repo pattern.
- **RAGE** — Rust MITM proxy. Wraps Claude Code's API calls with a network ACL, secret scrubbing, and command interception. Runs inside FORGE VMs unchanged when the operator drops a Linux RAGE binary into `~/.forge/rage/`.

## Things easy to get wrong

Categorised. Each entry is a hard-won lesson — read these before
making changes in the relevant area.

### vfkit / vsock

- **vfkit vsock flags are BARE** (`listen` / `connect`), not k=v
  (`listen=true` / `listen=false`). vfkit silently treats any
  `listen=…` form as the default `listen` mode and never binds the
  unix socket — exactly the trap that lost a few iterations of
  debugging. Same for `socketURL`: pass a bare path, never a
  `unix://` URL.
- **`gvproxy` MUST start before `vfkit`.** vfkit's
  `--device virtio-net,unixSocketPath=…` dials the unix socket at
  boot; if `gvproxy` hasn't bound it yet, the VM comes up with no NIC.
  `internal/env/{create,start}.go` enforces this ordering — don't
  reorder. The reverse on stop: `vfkit` (and the wedge-recovery
  paths) reap `gvproxy` + `forgejoproxy` too, otherwise orphan unix
  sockets block the next start.
- **`gvisor-tap-vsock`'s `ListenUnixgram` wants a URL, not a bare
  path.** It returns "unexpected scheme" on a plain path.
  `internal/gvproxy` normalises to `unixgram://<path>` so the rest of
  the codebase passes plain paths around. If you swap the library or
  call its transport layer directly, remember to add the scheme.

### Networking

- **Host↔VM connectivity must NEVER depend on the host IP routing
  table.** macOS hosts running a tunnel-all corporate VPN (Cisco
  AnyConnect being the case we've tested) intercept traffic via
  macOS's Network Extension API, BELOW the routing layer. Adding
  routes (even with sudo) appears to work in `route get` output but
  packets still leave through the VPN. The vsock-bridged SSH and
  Forgejo paths are the deliberate, VPN-proof workarounds. For VM
  outbound traffic (apt, k3s pulls, RAGE's API calls), the answer is
  `gvproxy` — every VM connection becomes a host-side `socket()`
  call, indistinguishable from a Mac browser to the VPN. **Never
  reintroduce vmnet-NAT mode.**
- **Don't hard-code `192.168.127.x`.** It's `gvproxy`'s default
  subnet today and likely will stay that way, but read it from
  `internal/env/ipalloc.go` or `internal/gvproxy` constants. The
  legacy `192.168.64.x` references that survived the migration are
  all in comments explaining historical context — don't bring them
  back as live values.

### cloud-init / bootstrap

- **`forge-bootstrap` runs `require_cmd` first — keep the list in
  lock-step with `images/base/build.sh`.** Anything bootstrap shells
  out to (curl, jq, git, socat, sudo, install, tar, base64, sha256sum,
  systemctl, python3 today) MUST be in the base image's apt install
  line. The dep check fails the boot fast with a clear error if
  anything's missing — without it, a stale base image silently
  produces broken envs. When you add a new runtime tool, edit BOTH
  the apt install line AND the `require_cmd …` argument list. The
  test asserts the list verbatim so divergence shows up in CI.
- **Bootstrap script ordering matters.** `forge-bootstrap` is
  `set -euo pipefail`; if any step fails, the chained `&&` between
  bootstrap and `forge-ready` in runcmd means the ready signal never
  fires and `forge env create` correctly times out → `crashed`.
  Stages: wait-for-route → require_cmd → swap file → pre-create
  `.config` and `.claude` → k3s install + KUBECONFIG profile.d →
  helm install → rage virtio-fs mount/copy/umount → claude-code
  install.sh + symlink. Don't tack things to the end without
  thinking about ordering.
- **Cloud-init `runcmd` is first-boot-only.** Anything that must run
  on every boot (forge-ssh-vsock socat, virtio-fs mounts, rage
  install on a re-imaged disk) belongs in a systemd unit, fstab, or
  `/etc/modules-load.d/` — NOT in `runcmd`. The first-boot-only
  behaviour is why `forge env start` waits on the SSH banner over
  `ssh.sock` instead of the vsock-ready signal.
- **Don't use cloud-init `write_files: defer:true` with a parent dir
  that doesn't exist yet.** On Debian trixie + cloud-init 25.x, a
  deferred write to `/home/forge/.claude/X` (when `.claude` doesn't
  exist) wedges `cloud-init-main` indefinitely at "Waiting on external
  services to complete before starting the final stage." `cloud-final`
  never starts → `runcmd` never runs → `/var/log/forge-bootstrap.log`
  is never created. The symptom is `forge env create` hanging at
  "Bootstrapping VM" with a perfectly-SSHable VM where cloud-init is
  parked. Workaround: emit the file from `forge-bootstrap` itself
  (which pre-creates `.claude` via `install -d`). The Claude Code
  statusline (`internal/cloudinit/files/statusline.sh`) uses this
  pattern — `base64 -d` heredoc inside the bootstrap script.
- **Swap is mandatory in the VM.** The claude-code native binary is
  ~250 MB and its `install` subcommand allocates further at runtime;
  with the 4 GB forge.yaml default and k3s already running, the
  install gets SIGKILL'd by the OOM killer ("Killed" with no other
  message). `forge-bootstrap` creates a 2 GB swap file at
  `/var/swap.img` BEFORE the heavy installs. Don't move that step
  later in the script — it has to land before the k3s install.
- **`claude.ai/install.sh` always downloads "latest" as its bootstrap
  binary.** Even when you pass a TARGET (e.g. `1.2.3` or `stable`),
  the script first fetches the most recent claude binary, then defers
  to TARGET via `binary install <TARGET>` for the persistent install.
  The transient bootstrap binary is verified against claude.ai's
  manifest but is unpinned by us. We accept this because (a)
  install.sh's content is sha256-pinned by us, so the wrapper logic
  can't be silently swapped; (b) the bootstrap binary's checksum
  verification chain is HTTPS+manifest-bound. If this trust gets
  revisited, the alternative is to bypass install.sh and curl
  `downloads.claude.ai/claude-code-releases/<version>/<platform>/claude`
  directly with our own pinned sha256 — same URLs the installer uses.
- **`install.sh`'s TARGET regex rejects `v`-prefixed versions.**
  `^(stable|latest|N.N.N(-suffix)?)$` — passing `v0.4.2` fails.
  The bootstrap strips a leading `v`
  (`CLAUDE_TARGET="${CLAUDE_VERSION#v}"`) so forge.yaml entries
  written in either style work. Don't add Go-side normalisation; the
  strip lives in the rendered shell so the wire format flows through
  unchanged for k3s and helm.

### File ownership / paths

- **`/home/forge/.config` must be pre-created as forge.** GNU
  `install -d -o forge -g forge /home/forge/.config/<sub>` creates
  intermediate parents with default ownership (root:root) and only
  applies `-o`/`-g` to the leaf. So a step like the rage install
  (which creates `.config/rage`) leaves `.config` itself root-owned,
  and the next tool that wants to write under it (`helm repo add`,
  kubectl plugin caches, anything XDG_CONFIG_HOME-aware) fails with
  "permission denied". `forge-bootstrap` runs `install -d -o forge
  -g forge -m 0755 /home/forge/.config` early to defuse this. Don't
  drop that line. Same applies to `.claude`.
- **Workspace UID matching.** The in-VM `forge` user gets
  `os.Getuid()` (the host user's UID) so files written on either side
  of the virtio-fs `workspace-share` are owned by the same numeric
  UID on both views. Don't drop this — without it, the agent in the
  VM can't write to host-created files and vice versa.

### SSH connect wrapper

- **`forge env connect` remote command must be ONE arg to ssh.**
  ssh joins remote-command args with spaces and sends as a single
  string; the remote outer shell tokenizes on the other side. Three-arg
  forms like `["bash", "-lc", "cd … && exec …"]` collapse on the wire
  to `bash -lc cd … && exec …`, which bash mis-reads (`-c="cd"` with
  `$0="…"`) and silently exits — connection closes with no error.
  Always pass the whole `bash -l -c '…'` payload as a single string
  with internal single-quotes so the inner script survives the
  second tokenizer.

### Statusline

- **The Claude Code statusline travels with the binary.**
  `internal/cloudinit/files/statusline.sh` is `go:embed`-ed and
  base64-encoded into the rendered cloud-init at template-render
  time. The blob is dropped via a `base64 -d > … <<'STATUSLINE_B64'`
  heredoc inside `forge-bootstrap`. A minimal
  `/home/forge/.claude/settings.json` next to it points claude at it.
  The script depends on `jq`, installed at base-image build time.
  To update the statusline, replace the embedded file and rebuild.
  To debug a misrendering bar, `touch /tmp/statusline-debug` inside
  the VM — the script will then append every render's input JSON to
  `/tmp/statusline-input.jsonl`. `rm` the marker to stop.

### Operational

- **Forgejo image is digest-pinned.** Don't change
  `internal/forgejo/pins.go` by hand — re-run
  `./scripts/refresh-pins.sh`.
- **`forge system start` has two modes.** `--mode existing` (connect
  to a running Forgejo) and `--mode new` (start a managed container at
  a probed free port). Both write `~/.forge/config.yaml`.
- **`forge system stop` is managed-mode only.** `disconnect` is
  config-only and works in both modes. They do different things —
  see the README's System (Forgejo) section.
- **Per-env Forgejo state survives `forge env destroy` by default.**
  The `<env>` user and its `workspace` repo persist for review
  history. Pass `--purge-forgejo` to remove them.
- **Forgejo runs in Docker on the host, not inside the VM.** k3s and
  the agent run inside the VM; Forgejo is a separate host-side
  container. The VM reaches it via the vsock-bridged forgejoproxy at
  `localhost:<forgejo-port>` (or via gvproxy's host-NAT at
  `192.168.127.254:<port>` — both work; the URL FORGE bakes into
  `.git-credentials` is `localhost:<port>` to match what the host
  clone records).
- **`forge env create` seeds project files into the new env's
  workspace.** After cloning the empty Forgejo repo, the create flow
  copies a curated short list of dotfiles (today:
  `.pre-commit-config.yaml`) from the host project root (the dir
  containing `forge.yaml`) into the workspace and pushes them as the
  seed commit on `main`. Extends via `cmd/env/create.go:seedFiles`.
- **`forge env destroy` removes `<envDir>/workspace`** — the user
  loses any uncommitted local changes. There's no "are you sure?"
  beyond the standard destroy prompt.
- **Token in `~/.forge/envs/<name>/cloud-init.iso`.** Cloud-init
  writes `/home/forge/.git-credentials` so VM-side `git push` works.
  The token is therefore embedded in the cidata ISO on disk.
  Acceptable for FORGE's threat model (personal dev tool on a trusted
  host) — but worth knowing if you're moving envs across machines.

## Project philosophy (inherited from CAGE)

- **We trust developers.** The tool protects them FROM bad actors,
  not from themselves.
- **Don't be a blocker.** Security should be invisible in default mode.
- **Hardened mode is opt-in friction.**
- **Test-driven development.** Tests first, code second.
