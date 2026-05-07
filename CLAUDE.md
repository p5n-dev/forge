# FORGE — AI assistant primer

## What this project is

FORGE is a VM-based sandbox for running AI coding agents (Claude Code) in isolation, with native Kubernetes support. It is the VM-based sibling of [CAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/cage) (Docker-based sandboxing) and runs alongside [RAGE](https://sbp.gitlab.schubergphilis.com/Security/tools/rage) (API-level guardrails proxy).

- **CAGE** = Contained Agentic Guarded Environment (Docker)
- **RAGE** = Runtime Agent Guardrails Emulator (API proxy)
- **FORGE** = Federated Orchestrated Runtime Guarded Environment (VM)

CAGE is great for general coding work, but Kubernetes can't run safely inside a Docker container — it requires privileged containers, nested namespaces, and a shared kernel, which undermine the isolation model. FORGE replaces the container with an Apple Virtualization.framework VM so k3s runs natively on a real kernel.

## Status

**MVP complete.** macOS Apple Silicon only. Linux/KVM, OCI registry distribution, and multi-node k3s clusters are tracked as post-MVP.

Recent work (current focus area, in rough order):

- **gvproxy userspace networking.** vfkit's `--device virtio-net,nat` (vmnet shared mode) is gone. Replaced by `--device virtio-net,unixSocketPath=<envDir>/net.sock` connected to a per-env [`gvisor-tap-vsock`](https://github.com/containers/gvisor-tap-vsock) instance running as a sister process to vfkit. The VM's subnet is now `192.168.127.0/24` (gvproxy's default), and every outbound connection becomes a host-side `socket()` syscall — indistinguishable from a Mac browser to the corp VPN's NEPacketTunnelProvider, so VMs work with the VPN connected. Same architecture OrbStack/Podman Machine/WSL2 use. See **Networking model** below and `internal/gvproxy/`.
- **`forge env connect` lands in `/home/forge/workspace`.** Both `--bash` and the default rage variant wrap the remote command in `bash -l -c 'cd /home/forge/workspace && exec <cmd>'` so (a) `/etc/profile.d/*` sources for PATH+KUBECONFIG, and (b) the agent starts inside the env's Forgejo project tree (the virtio-fs `workspace-share`), not `$HOME`. Connect now also accepts envs in `starting` state so the user can ssh in mid-bootstrap to tail `/var/log/forge-bootstrap.log`.
- **claude-code installs via Anthropic's official `claude.ai/install.sh`.** No more pnpm/npm/Node.js in the bootstrap path — the official installer downloads a single-file native binary verified against claude.ai's manifest.json. We pre-fetch the script, verify its sha256 (pinned by `claudeCodeInstallerSHA256` in `internal/cloudinit/userdata.go`, refreshed by `scripts/refresh-pins.sh`), then run it as the forge user with the forge.yaml-supplied version as TARGET. After install, the binary lands at `/home/forge/.local/bin/claude` and we symlink it to `/usr/local/bin/claude` so non-login child processes (rage spawning claude) find it via the default Debian PATH.
- **k3s install is k3s-readable.** The k3s install passes `K3S_KUBECONFIG_MODE=644` and a `/etc/profile.d/k3s.sh` exports `KUBECONFIG`, so `kubectl` works as the forge user without sudo.
- **helm in the bootstrap.** Same model as the k3s and claude-code installers: pre-fetch `helm/helm/scripts/get-helm-3`, verify against `helmInstallerSHA256`, then exec with `DESIRED_VERSION=$HELM_VERSION`. Helm version is configurable per-env via `forge.yaml`'s `bootstrap.helm`. Lands in `/usr/local/bin/helm` (the installer's default) so it's on PATH for the forge user immediately.
- **VPN-immune host↔guest plumbing for SSH and Forgejo.** `forge env connect` rides a vsock-bridged Unix socket end-to-end (host-unix → vfkit → guest-vsock → in-VM socat → sshd). The same pattern is used for in-VM `git push`: `internal/forgejoproxy` binds a per-env unix socket and forwards to the host's Forgejo TCP endpoint, with vfkit's third virtio-vsock device dialing the socket whenever the in-VM socat unit forwards `127.0.0.1:<forgejo>` traffic. Layered defense — vsock paths never touch the IP stack so they survive any future netstack issue.
- **Declarative cloud-init for the forge user's git config.** `/home/forge/.gitconfig` is written via `write_files: defer:true` (not `git config --global` in runcmd, which raced with cloud-init's user-mod stage and left the file unwritable). User identity is pre-baked: `user.name=<env>`, `user.email=<env>@forge.local` (matches the per-env Forgejo user). `chown -R forge:forge /home/forge` runs first in runcmd to defensively fix a Debian-cloud-arm64 quirk where the home dir landed as `root:root`. TERM=xterm-256color fallback appended to `.bashrc` for ghostty.
- **`forge env start` UX + correctness.** Boots show per-step progress (rendering / booting / waiting for SSH). The readiness probe is the SSH banner over `ssh.sock`, NOT the boot-ready vsock signal — the latter is first-boot-only (cloud-init `runcmd` + the base image's `forge-ready.service` both gate on `/var/lib/forge/ready.done`).
- **`forge env stop --force`** unwedges envs stuck in `starting`/`stopping`/`crashed`. Failed `Start`s and failed `Create`s transition to `crashed` and reap vfkit + gvproxy + forgejoproxy.
- **`forge doctor`** reports vfkit health, per-env reachability via the ssh socket, and per-env net.sock presence. Exits non-zero on any FAIL.
- **`forge init` picks up a project-local `rage/` directory** and copies it into `~/.forge/rage` (CAGE convention, see `docs/cage-README.md`). `--force` overwrites both `forge.yaml` and `~/.forge/rage`.
- **`forge env create` seeds project files into the new env's workspace.** After cloning the empty Forgejo repo, the create flow copies a curated short list of dotfiles (today: `.pre-commit-config.yaml`) from the host project root (the dir containing `forge.yaml`) into the workspace and pushes them as the seed commit on `main`. This both (a) propagates project-wide tooling config to every env, and (b) gives the freshly created Forgejo repo a real default branch so subsequent VM-side clones aren't unborn-HEAD. The seed list lives in `cmd/env/create.go:seedFiles` — extend it there. No-op when `forge.yaml` wasn't found (built-in defaults path).
- **virtio-fs shares for rage and the workspace.** `~/.forge/rage` is shared read-only as `rage-share`; `<envDir>/workspace` is shared read-write as `workspace-share` and mounted at `/home/forge/workspace` via fstab. Forgejo repo is cloned into the workspace at create time.
- **HostUID matching.** The in-VM `forge` user gets `os.Getuid()` as its UID so files in the workspace share have identical ownership on host and guest.

## Supply-chain hardening — HARD REQUIREMENT, ALWAYS

The user is *strict* on supply-chain attack prevention. These are non-negotiable rules for every change, every PR, every dependency, forever:

1. **Pin every external dependency by version AND content hash.** Versions alone are not enough — most registries allow tag mutation. We pin the digest/SHA256 too so a swapped artifact fails verification.
   - Container images: `image:tag@sha256:...` form (see `internal/forgejo/pins.go`, `images/base/Dockerfile`).
   - Standalone binaries fetched at runtime: pin both the version constant and a `sha256` constant, and have the script that fetches them run `sha256sum -c` before executing (see `nodejsVersion`/`nodejsSHA256` patterns historically; today the runtime fetches are the k3s and claude-code install scripts in `internal/cloudinit/userdata.go`, both verified before exec).
   - Vendor installer scripts (the `curl | sh` family — `claude.ai/install.sh`, `get.k3s.io`, anchore/syft `install.sh`): pin the SCRIPT content's sha256. The version of the underlying tool is then chosen via the script's TARGET arg / env var, and the script's own checksum chain verifies the binary it downloads. Two-layer trust: our pin protects the wrapper; the wrapper protects the artefact.
2. **Apply a soak before adopting a new version.** `SOAK_DAYS=14` by default — only pin releases that have been published at least that long. This is enforced by `scripts/refresh-pins.sh`.
3. **`scripts/refresh-pins.sh` is the only legitimate way to change a pin.** Do not hand-edit the pinned constants. The script enforces the soak rule and recomputes the hash from the actual artifact. If you add a new pinned dependency, you must also extend the script to refresh it — otherwise the pin will rot silently.
4. **Sentinel comments matter.** Every pinned constant in source has a `// pin:<name>-version` or `// pin:<name>-sha256` trailing comment. The refresh script uses these as sed anchors. Keep them in any new constant you add.
5. **`curl … | sh -` is a smell — only tolerated as a last resort.** Pin the script's content too, like we now do for `get.k3s.io` and the syft installer. The remaining "tolerated" exception is genuinely unrecoverable cases (e.g. an installer that runs out of an HTTP redirect chain we don't control). RAGE used to be in this bucket; it isn't anymore (the binary is virtio-fs-shared from the host).
6. **`apt-get install` is OK without our own version+sha pin** — but only because:
   - The base/builder image is digest-pinned (`debian:13-slim@sha256:…` or `ubuntu-24.04-arm` GH runner) — so the apt sources list and trusted-key configuration are themselves frozen.
   - Apt verifies every package against Debian's GPG-signed `Release` file before install. That's a strong integrity layer we'd be duplicating, not adding to.
   - Pinning every package version would create disproportionate maintenance churn (Debian rolls security patches into the stable point release; pins would block them).
   The existing apt usages (`images/base/Dockerfile`, `.github/workflows/image.yml`) are all in the **build pipeline**, never at VM runtime. **Do not introduce `apt-get install` into the in-VM bootstrap path** — runtime fetches go through the version+sha pin process.

When asked to add a new dependency, the workflow is: (a) add a placeholder pin with sentinel comments → (b) extend `scripts/refresh-pins.sh` to populate it → (c) run the script → (d) commit the populated values together with the dependency change.

## Read these first

When you start a session, skim these in order — they describe what's currently true, not what was once planned:

1. [`README.md`](./README.md) — user-facing entry point: requirements, quick start, full CLI reference, on-disk layout
2. [`docs/spec.md`](./docs/spec.md) — architecture spec, decision log, package layout, security model
3. `forge.yaml` and `~/.forge/config.yaml` examples in the README — the two-level config model

## Reference projects (sibling tools)

These live on the user's macOS host (not visible from inside this Linux container — ask the user to fetch specific files by name if needed):

- **CAGE** — Go (cobra/zerolog/lipgloss). Useful files: `CLAUDE.md`, `docs/spec-rewrite.md`, `internal/template/embedded/templates/base.Dockerfile`, `docs/architecture.md`, `docs/adr/`. FORGE inherits CAGE's CLI shape and philosophy.
- **RAGE** — Rust MITM proxy. Useful files: `CLAUDE.md`, `docs/ISOLATION.md`, `rage.toml`. RAGE runs inside FORGE VMs unchanged.

## Project philosophy (inherited from CAGE)

- **We trust developers.** The tool protects them FROM bad actors, not from themselves.
- **Don't be a blocker.** Security should be invisible in default mode.
- **Hardened mode is opt-in friction.**
- **Test-driven development.** Tests first, code second.

## Networking model

FORGE has **four distinct host↔guest channels** per env. Three ride vfkit's
virtio-vsock devices (Unix sockets) and one rides a virtio-net device
(also a Unix socket — to a userspace TCP/IP stack). Two virtio-fs shares
on top of that. **None of them traverse the macOS host's IP routing
table to reach the VM**, which is the point — they all work with the
corp Cisco AnyConnect VPN connected, even though the VPN's
NEPacketTunnelProvider would otherwise drop vmnet-NAT'd packets.

1. **`net.sock` — gvproxy userspace netstack (general egress)**
   - vfkit: `--device virtio-net,unixSocketPath=<envDir>/net.sock,mac=...`
   - Host process: `forge env _net` (subprocess, runs the
     `gvisor-tap-vsock` library — see `internal/gvproxy/`). Binds the
     unixgram socket, runs DHCP/DNS/NAT in userspace, translates VM
     packets to host `socket()` calls. Subnet `192.168.127.0/24`,
     gateway `.1`, host-loopback magic IP `.254` (NATed to `127.0.0.1`).
   - VM view: a normal NIC with internet, DNS, the works. `apt`, `curl`,
     `git`, `ping`, k3s registry pulls, rage outbound — all work with
     VPN on because they look like Mac-process traffic to the policy
     engine. Same model OrbStack uses.
   - This is the **first time the VM gets real outbound networking** in
     FORGE; the previous vmnet-NAT mode was VPN-incompatible.

2. **`ssh.sock` — vsock-bridged SSH (host → guest)**
   - vfkit: `--device virtio-vsock,port=22,socketURL=<envDir>/ssh.sock,connect`
   - VM-side: `forge-ssh-vsock.service` runs
     `socat VSOCK-LISTEN:22 ... TCP:127.0.0.1:22` to bridge to sshd.
   - `forge env connect` SSHes via `-o ProxyCommand="nc -U ssh.sock"`.

3. **`forgejo.sock` — vsock-bridged Forgejo (guest → host)**
   - vfkit: `--device virtio-vsock,port=4000,socketURL=<envDir>/forgejo.sock,listen`
   - Host process: `forge env _proxy` (subprocess — see
     `internal/forgejoproxy/`). Forwards each accepted connection to
     `127.0.0.1:<forgejo-port>` on the host.
   - VM-side: `forge-forgejo-vsock.service` runs
     `socat TCP-LISTEN:4000,bind=127.0.0.1 VSOCK-CONNECT:2:4000` so
     `localhost:4000` inside the VM reaches the host's Forgejo.
   - Layered alongside gvproxy: `git push` would also work via gvproxy's
     loopback NAT (`192.168.127.254`), but the vsock path is more
     defensive — it never touches the IP stack at all, so any future
     netstack hiccup leaves it intact.

4. **Boot-ready vsock signal** (`<envDir>/vsock.sock`, port 1234)
   - First-boot-only. The base image's `forge-ready.service` and the
     cloud-init `runcmd` both gate on `/var/lib/forge/ready.done`, so
     this fires once. `forge env create` blocks on it; `forge env start`
     deliberately doesn't open the listener (it'd never get dialed).

**virtio-fs shares (filesystem)** — two shares per env:
   - `rage-share` ← `~/.forge/rage` (read-only). Cloud-init's
     `forge-bootstrap` copies `rage-$(uname -m)-linux` to `/usr/local/bin/rage`
     and `rage.toml` to `/home/forge/.config/rage/rage.toml`. **Rage is NOT
     fetched from the network**; the user provides the binary per the CAGE
     convention.
   - `workspace-share` ← `<envDir>/workspace` (read-write). Mounted at
     `/home/forge/workspace` via `/etc/fstab` so it survives reboots. The
     env's Forgejo repo is `git clone`d into this directory at create time;
     host and the in-VM agent edit the same tree.

vfkit syntax notes:
- vsock device flags are **bare** (`listen` / `connect`), not k=v
  (`listen=true` / `listen=false`). vfkit silently treats any `listen=…`
  form as the default `listen` mode and never binds the unix socket —
  exactly the trap that lost a few iterations of debugging.
- `socketURL` for vsock devices takes a plain filesystem path
  (no `unix://` prefix).
- For the net device specifically, gvisor-tap-vsock's `transport.ListenUnixgram`
  parses its argument as a URL — bare paths fail with "unexpected
  scheme". `internal/gvproxy` normalises to `unixgram://<path>` at the
  wrapper boundary so callers can pass plain paths.

Lifecycle ordering matters on `forge env create` and `forge env start`:
**gvproxy must come up before vfkit** (vfkit's virtio-net device dials
`net.sock` at boot; if nothing's listening, the VM has no NIC). The
forgejoproxy is started right after, then vfkit. `forge env stop` (and
the half-booted-VM cleanup paths) reap them in reverse.

## Code-style conventions

- Go, with `cobra` for CLI, `zerolog` for logging, `lipgloss` for terminal UI.
- Terse code. Short functions. No premature abstractions and no half-finished implementations.
- Comments explain *why*, not *what*. Default to no comments.
- Pre-commit hooks (`.pre-commit-config.yaml`) run gofmt, goimports, golangci-lint, build, and `go test -short` on every commit. CI runs the same checks plus the integration suite on a macOS runner.
- Shell scripts must be portable across BSD (macOS host) and GNU (Linux CI / build container) — use `[[:space:]]` not `\s`, watch for `date -d` vs `date -j -f`, etc.

## Things that are easy to get wrong

- **Forgejo image is digest-pinned.** Don't change `internal/forgejo/pins.go` by hand — re-run `./scripts/refresh-pins.sh`, which enforces the 14-day soak rule.
- **`forge system start` has two modes.** `--mode existing` (connect to a running Forgejo, e.g. CAGE's) and `--mode new` (start a managed container at a probed free port). Both write `~/.forge/config.yaml`.
- **`forge system stop` is managed-mode only.** `disconnect` is config-only and works in both modes. They do different things — see the README's System (Forgejo) section.
- **Per-env Forgejo state survives `forge env destroy` by default.** The `<env>` user and its `workspace` repo persist for review history. Pass `--purge-forgejo` to remove them.
- **Forgejo runs in Docker on the host, not inside the VM.** k3s and the agent run inside the VM; Forgejo is a separate host-side container. The VM reaches it via the vsock-bridged forgejoproxy at `localhost:<forgejo-port>` (or via gvproxy's host-NAT at `192.168.127.254:<port>` — both work; the URL FORGE bakes into `.git-credentials` is `localhost:<port>` to match what the host clone records).
- **vfkit vsock flags are BARE** (`listen` / `connect`), not k=v (`listen=true` / `listen=false`). vfkit silently treats any `listen=…` form as the default `listen` mode and never binds the unix socket — exactly the trap that lost a few iterations of debugging. Same for `socketURL`: pass a bare path, never a `unix://` URL.
- **Cloud-init `runcmd` is first-boot-only.** Anything that must happen on every boot (forge-ssh-vsock socat, virtio-fs mounts, rage install on a re-imaged disk) belongs in a systemd unit, fstab, or `/etc/modules-load.d/` — NOT in `runcmd`. The first-boot-only behaviour is why `forge env start` waits on the SSH banner over `ssh.sock` instead of the vsock-ready signal.
- **Host↔VM connectivity must NEVER depend on the host IP routing table.** Schuberg Philis Macs run a Cisco VPN client that intercepts traffic via macOS's Network Extension API, BELOW the routing layer. Adding routes (even with sudo) appears to work in `route get` output but packets still leave through the VPN. The vsock-bridged SSH and Forgejo paths are the deliberate, VPN-proof workarounds — keep them that way. For VM **outbound** traffic (apt, k3s pulls, rage's API calls), the answer is gvproxy: a userspace netstack on the host where every VM connection becomes a host-side `socket()` call, indistinguishable from a Mac browser to the VPN. Never reintroduce vmnet-NAT mode.
- **gvproxy MUST start before vfkit.** vfkit's `--device virtio-net,unixSocketPath=…` dials the unix socket at boot; if gvproxy hasn't bound it yet, the VM comes up with no NIC. `internal/env/{create,start}.go` enforces this ordering — don't reorder. The reverse on stop: vfkit (or the wedge-recovery path) reaps gvproxy + forgejoproxy too, otherwise the orphan unix sockets block the next start.
- **gvisor-tap-vsock's `ListenUnixgram` wants a URL, not a bare path.** It returns "unexpected scheme" on a plain path. `internal/gvproxy` normalises to `unixgram://<path>` so the rest of the codebase passes plain paths around. If you swap the library or call its transport layer directly, remember to add the scheme.
- **Don't hard-code `192.168.127.x` in code.** It's gvproxy's default subnet today and likely will stay that way, but read it from `internal/env/ipalloc.go` or `internal/gvproxy` constants. The legacy `192.168.64.x` references that survived the migration are all in comments explaining historical context — don't bring them back as live values.
- **Bootstrap script ordering matters.** `forge-bootstrap` is `set -euo pipefail`; if any step fails, the chained `&&` between bootstrap and `forge-ready` in runcmd means the ready signal never fires and `forge env create` correctly times out → `crashed`. Don't insert anything into the chain that breaks this property. Stages: wait-for-route, k3s install, KUBECONFIG profile.d (depends on the k3s.yaml path existing), rage virtio-fs mount + copy + umount, claude-code install.sh (runs as forge user, symlinks /home/forge/.local/bin/claude → /usr/local/bin/claude). New steps belong at the appropriate point; don't tack things to the end without thinking about it.
- **claude.ai/install.sh always downloads "latest" as its bootstrap binary.** Even when you pass a TARGET (e.g. `1.2.3` or `stable`), the script first fetches the most recent claude binary, then defers to TARGET via `binary install <TARGET>` for the persistent install. The transient bootstrap binary is verified against claude.ai's manifest, but is unpinned by us. We accept this because (a) install.sh's content is sha256-pinned by us, so the wrapper logic can't be silently swapped; (b) the bootstrap binary's checksum verification chain is HTTPS+manifest-bound. If this trust gets revisited, the alternative is to bypass install.sh and curl `downloads.claude.ai/claude-code-releases/<version>/<platform>/claude` directly with our own pinned sha256 — same URLs the installer uses.
- **install.sh's TARGET regex rejects `v`-prefixed versions.** `^(stable|latest|N.N.N(-suffix)?)$` — passing `v0.4.2` fails. The bootstrap strips a leading `v` (`CLAUDE_TARGET="${CLAUDE_VERSION#v}"`) so forge.yaml entries written in either style work. Don't add Go-side normalisation; the strip lives in the rendered shell so the wire format from forge.yaml flows through unchanged for k3s and rage.
- **Claude Code statusline travels with the binary.** `internal/cloudinit/files/statusline.sh` is `go:embed`-ed and base64-encoded into the rendered cloud-init at template-render time. The blob is dropped via a `base64 -d > … <<'STATUSLINE_B64'` heredoc inside `forge-bootstrap`, NOT via cloud-init `write_files` — see the next bullet for why. A minimal `/home/forge/.claude/settings.json` next to it points claude at it. The script depends on `jq`, installed at base-image build time (`images/base/build.sh`). To update the statusline, replace `internal/cloudinit/files/statusline.sh` and rebuild. To debug a misrendering bar (e.g. claude code's status JSON shape drifts between releases), `touch /tmp/statusline-debug` inside the VM — the script will then append every render's input JSON to `/tmp/statusline-input.jsonl`. `rm` the marker to stop.
- **`forge-bootstrap` runs `require_cmd` first — keep the list in lock-step with `images/base/build.sh`.** Anything bootstrap shells out to (curl, jq, git, socat, sudo, install, tar, base64, sha256sum, systemctl, python3 today) MUST be in the base image's apt install line. The dep check at the top of the bootstrap script fails the boot with a clear error if anything's missing — without it, a stale base image silently produces broken envs (the actual incident: `jq` got added to the statusline before being added to the base image's apt install, and the statusline silently fell back to "claude › 0/0 0% › $0.0000" while everything else "worked"). When you add a new runtime tool, edit BOTH `images/base/build.sh`'s `--install` line AND the `require_cmd …` argument list in `internal/cloudinit/userdata.go`. The test asserts the require_cmd list verbatim so a divergence shows up in CI.
- **Don't use cloud-init `write_files: defer: true` with a parent dir that doesn't exist yet.** Specifically on Debian trixie + cloud-init 25.x, a deferred write to `/home/forge/.claude/X` (when `.claude` doesn't exist) wedges `cloud-init-main` indefinitely at "Waiting on external services to complete before starting the final stage." `cloud-final.service` never starts → `runcmd` never runs → `/var/log/forge-bootstrap.log` is never created → `forge env logs` fails with "no such file." The symptom is a `forge env create` that hangs at "Bootstrapping VM" and a VM you can SSH into where everything looks fine except cloud-init is parked. Workaround used here: emit the file from `forge-bootstrap` (which already pre-creates `.claude` via `install -d`) instead of from `write_files`. If you need to add another large/parent-dir-creating dotfile in future, drop it the same way — `base64 -d` heredoc + `chown forge:forge`.
- **`/home/forge/.config` must be pre-created as forge.** GNU `install -d -o forge -g forge /home/forge/.config/<sub>` creates intermediate parents with default ownership (root:root) and only applies `-o`/`-g` to the leaf. So a step like the rage install (which creates `.config/rage`) leaves `.config` itself root-owned, and the next tool that wants to write under it (`helm repo add`, kubectl plugin caches, anything XDG_CONFIG_HOME-aware) fails with "permission denied". `forge-bootstrap` runs `install -d -o forge -g forge -m 0755 /home/forge/.config` early to defuse this. Don't drop that line.
- **Swap is mandatory in the VM.** The claude-code native binary is ~250 MB and its `install` subcommand allocates further at runtime; with the 4 GB forge.yaml default and k3s already running, the install gets SIGKILL'd by the OOM killer ("Killed" with no other message). `forge-bootstrap` creates a 2 GB swap file at `/var/swap.img` BEFORE the heavy installs and adds an fstab entry. Don't move this step later in the script — it has to land before the k3s install for the same reason.
- **`forge env connect` remote command must be ONE arg to ssh.** ssh joins remote-command args with spaces and sends as a single string; the remote outer shell tokenizes on the other side. Three-arg forms like `["bash", "-lc", "cd … && exec …"]` collapse on the wire to `bash -lc cd … && exec …`, which bash mis-reads (`-c="cd"` with `$0="…"`) and silently exits — connection closes with no error. Always pass the whole `bash -l -c '…'` payload as a single string with internal single-quotes so the inner script survives the second tokenizer.
- **Workspace UID matching matters.** The in-VM `forge` user gets `os.Getuid()` (the host user's UID) so files written on either side of the virtio-fs `workspace-share` are owned by the same numeric UID on both views. Don't drop this — without it, the agent in the VM can't write to host-created files and vice versa.
- **Token in `~/.forge/envs/<name>/cloud-init.iso`.** Cloud-init writes `/home/forge/.git-credentials` so VM-side `git push` works. The token is therefore embedded in the cidata ISO on disk. Acceptable for FORGE's threat model (personal dev tool on a trusted host) — but worth knowing if you're handling envs across machines.
- **`forge env destroy` removes `<envDir>/workspace`** — the user loses any uncommitted local changes. There's no "are you sure?" beyond the standard destroy prompt; document this if you add features that put unique state in the workspace.
