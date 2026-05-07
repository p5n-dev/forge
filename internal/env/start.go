package env

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/p5n-dev/forge/internal/cloudinit"
	"github.com/p5n-dev/forge/internal/progress"
	"github.com/p5n-dev/forge/internal/vm"
)

// StartInput is the user-facing configuration for `forge env start`.
//
// Resource sizing (CPUs, MemoryMB) and MAC address are not duplicated on
// the input — Start reads them from the persisted state.json so the env
// starts with the same shape it was created with.
type StartInput struct {
	Name       string
	EnvBaseDir string // ~/.forge/envs (already expanded)
	// Bootstrap versions are re-templated into cloud-init at boot time.
	// They typically come from forge.yaml + global config so a `forge env
	// start` after a config bump uses the new versions.
	K3sVersion    string
	RageVersion   string
	ClaudeVersion string
	HelmVersion   string
	// ForgejoRemoteURL is the VM-facing clone URL (already host-rewritten
	// for NAT, see `cmd/env/create.go`). May be empty if the env doesn't
	// use Forgejo.
	ForgejoRemoteURL string
	// RageShareDir mirrors CreateInput.RageShareDir: the host directory
	// to expose as the rage-share virtio-fs mount in the guest.
	RageShareDir string
	// HostUID, ForgejoHostBase, ForgejoVsockPort, and ForgejoUser/Token
	// mirror CreateInput's same-named fields so cloud-init re-renders
	// consistently across `forge env create` and `forge env start`.
	// They only matter on first boot in practice (cloud-init runcmd is
	// instance-id-gated) but rendering them keeps env restarts on a
	// fresh disk image well-defined.
	HostUID          int
	ForgejoHostBase  string
	ForgejoVsockPort int
	ForgejoUser      string
	ForgejoToken     string
	// ForgejoProxyTarget is what internal/forgejoproxy dials on the
	// host (typically "127.0.0.1:<port>"). Empty → proxy isn't started.
	ForgejoProxyTarget string
	// Out is where Start prints user-visible messages. Optional.
	Out io.Writer
}

// StartDeps groups the side-effecting collaborators of Start.
type StartDeps struct {
	Runner      vm.Runner
	NetRunner   NetRunner
	ProxyRunner ProxyRunner
	WriteISO    func(outPath string, userData, metaData, networkConfig []byte) error
	// WaitForSSH polls the host-side ssh socket until the in-VM sshd is
	// reachable end-to-end. Tests inject a fake; production uses a thin
	// wrapper around env.WaitForSSH.
	WaitForSSH func(ctx context.Context, sshSockPath string) error
	// Progress reports per-step status to the user. nil → progress.Nop().
	Progress progress.Progress
}

// StartResult is what Start returns on success.
type StartResult struct {
	State *vm.State
}

// Start boots a stopped or crashed environment.
//
// It reuses the existing `disk.img`, regenerates cloud-init (no-op on
// the guest if instance-id matches, but cheap and means a forge.yaml
// bump applies on the next fresh env), starts vfkit, and then polls
// the vsock-bridged SSH socket until the VM is reachable end-to-end.
//
// The boot-ready vsock signal that `env.Create` waits on is
// deliberately NOT used here: it is first-boot-only (cloud-init's
// runcmd and the base image's forge-ready.service both gate on
// /var/lib/forge/ready.done), so it would never fire on a restart.
//
// `start` is also the recovery path for `crashed` envs: a stale
// `vfkit.pid` from the previous run is removed before the new vfkit
// subprocess is launched.
func Start(ctx context.Context, in StartInput, deps StartDeps) (*StartResult, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("env name is required")
	}
	if in.EnvBaseDir == "" {
		return nil, fmt.Errorf("env base dir is required")
	}
	if in.K3sVersion == "" || in.RageVersion == "" || in.ClaudeVersion == "" || in.HelmVersion == "" {
		return nil, fmt.Errorf("bootstrap versions are required (k3s, rage, claude_code, helm)")
	}

	prog := deps.Progress
	if prog == nil {
		prog = progress.Nop()
	}

	envDir := filepath.Join(in.EnvBaseDir, in.Name)
	if _, err := os.Stat(envDir); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("env %q not found", in.Name)
		}
		return nil, fmt.Errorf("statting env dir: %w", err)
	}

	state, err := vm.LoadState(envDir)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}

	// Only stopped/crashed envs can be started. The state machine in
	// vm.State.Transition will catch creating/running/starting/stopping
	// too, but giving a tailored message here is friendlier.
	if state.Status != vm.StatusStopped && state.Status != vm.StatusCrashed {
		return nil, fmt.Errorf("env %q is %s — only stopped or crashed envs can be started", in.Name, state.Status)
	}

	// Crashed recovery: drop the stale pid file so the runner doesn't
	// confuse the dead PID with a live one. os.Remove returns
	// ENOENT-style errors when the file is already gone — those are fine.
	pidPath := filepath.Join(envDir, "vfkit.pid")
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale pid file: %w", err)
	}

	pubKey := filepath.Join(envDir, "id_ed25519.pub")
	pubBytes, err := os.ReadFile(pubKey)
	if err != nil {
		return nil, fmt.Errorf("reading public key: %w", err)
	}

	done := prog.Step("Rendering cloud-init")
	userData, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              in.Name,
		AuthorizedKey:     trimSpace(string(pubBytes)),
		K3sVersion:        in.K3sVersion,
		RageVersion:       in.RageVersion,
		ClaudeCodeVersion: in.ClaudeVersion,
		HelmVersion:       in.HelmVersion,
		ForgejoRemoteURL:  in.ForgejoRemoteURL,
		HostUID:           in.HostUID,
		ForgejoHostBase:   in.ForgejoHostBase,
		ForgejoVsockPort:  in.ForgejoVsockPort,
		ForgejoUser:       in.ForgejoUser,
		ForgejoToken:      in.ForgejoToken,
	})
	if err != nil {
		done(err)
		return nil, fmt.Errorf("rendering user-data: %w", err)
	}
	metaData := cloudinit.RenderMetaData(in.Name)
	if state.IP == "" {
		// Pre-static-IP envs (created before the DHCP→static migration)
		// won't have a recorded IP. Allocate one now.
		ip, err := AllocateIP(in.EnvBaseDir)
		if err != nil {
			done(err)
			return nil, fmt.Errorf("allocating IP: %w", err)
		}
		state.IP = ip
	}
	networkConfig, err := cloudinit.RenderNetworkConfig(cloudinit.NetworkConfigInput{
		Address: state.IP,
		Prefix:  NetworkPrefix,
		Gateway: NetworkGateway,
		DNS:     []string{NetworkDNSPrimary, NetworkDNSFallback},
	})
	if err != nil {
		done(err)
		return nil, fmt.Errorf("rendering network-config: %w", err)
	}

	isoPath := filepath.Join(envDir, "cloud-init.iso")
	if err := deps.WriteISO(isoPath, userData, metaData, networkConfig); err != nil {
		done(err)
		return nil, fmt.Errorf("writing cloud-init iso: %w", err)
	}

	// vfkit refuses to bind a vsock socketURL whose path already exists.
	// Nuke any leftover ssh.sock from the previous boot before handing
	// the path to the runner.
	sshSockPath := SSHSocketPath(envDir)
	if err := os.Remove(sshSockPath); err != nil && !os.IsNotExist(err) {
		done(err)
		return nil, fmt.Errorf("removing stale ssh socket: %w", err)
	}
	done(nil)

	done = prog.Step(fmt.Sprintf("Booting VM (%d vCPU, %d MB RAM)", state.CPUs, state.Memory))

	// Mark starting before launching the subprocess — `forge env list`
	// running in parallel should see the in-progress state.
	if err := state.Transition(vm.StatusStarting); err != nil {
		done(err)
		return nil, fmt.Errorf("transitioning to starting: %w", err)
	}
	if err := state.Save(envDir); err != nil {
		done(err)
		return nil, fmt.Errorf("saving state: %w", err)
	}

	startOpts := vm.StartOptions{
		DiskPath:        filepath.Join(envDir, "disk.img"),
		CloudInitISO:    isoPath,
		CPUs:            state.CPUs,
		MemoryMB:        state.Memory,
		MAC:             state.MAC,
		NetSocketPath:   NetSocketPath(envDir),
		EFIVarStorePath: filepath.Join(envDir, "efi-vars"),
		// We deliberately omit VsockSocketPath: the boot-ready signal
		// is first-boot-only, so on `start` there's nothing to wait
		// for on that channel and we'd just be allocating a socket
		// that never gets dialed.
		SSHSocketPath:     sshSockPath,
		RageShareDir:      in.RageShareDir,
		WorkspaceShareDir: WorkspaceDir(envDir),
		ForgejoSocketPath: ForgejoSocketPath(envDir),
		ForgejoVsockPort:  in.ForgejoVsockPort,
	}

	// gvproxy must be listening before vfkit dials its virtio-net
	// unix socket. See create.go for the full rationale.
	if deps.NetRunner != nil {
		if err := deps.NetRunner.Start(ctx, envDir); err != nil {
			done(err)
			return nil, fmt.Errorf("starting gvproxy: %w", err)
		}
	}
	if deps.ProxyRunner != nil && in.ForgejoVsockPort != 0 && in.ForgejoProxyTarget != "" {
		if err := deps.ProxyRunner.Start(ctx, envDir, in.ForgejoProxyTarget); err != nil {
			done(err)
			if deps.NetRunner != nil {
				_ = deps.NetRunner.Stop(ctx, envDir)
			}
			return nil, fmt.Errorf("starting forgejo proxy: %w", err)
		}
	}
	if err := deps.Runner.Start(ctx, envDir, startOpts); err != nil {
		done(err)
		if deps.ProxyRunner != nil {
			_ = deps.ProxyRunner.Stop(ctx, envDir)
		}
		if deps.NetRunner != nil {
			_ = deps.NetRunner.Stop(ctx, envDir)
		}
		return nil, fmt.Errorf("starting vfkit: %w", err)
	}
	done(nil)

	done = prog.Step("Waiting for SSH (≈10–15s on a warm boot)")
	if err := deps.WaitForSSH(ctx, sshSockPath); err != nil {
		done(err)
		// Reap the half-booted vfkit and the host-side proxy, then
		// mark crashed so the user sees a clear failure in
		// `forge env list` and can retry (`forge env start`) without
		// needing `--force`. Best-effort: if either step also errors,
		// we still surface the original WaitForSSH failure.
		_ = deps.Runner.Stop(ctx, envDir)
		if deps.ProxyRunner != nil {
			_ = deps.ProxyRunner.Stop(ctx, envDir)
		}
		if deps.NetRunner != nil {
			_ = deps.NetRunner.Stop(ctx, envDir)
		}
		state.Status = vm.StatusCrashed
		_ = state.Save(envDir)
		return nil, fmt.Errorf("waiting for SSH: %w", err)
	}
	done(nil)

	if err := state.Transition(vm.StatusRunning); err != nil {
		return nil, fmt.Errorf("transitioning to running: %w", err)
	}
	if err := state.Save(envDir); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	if in.Out != nil {
		_, _ = fmt.Fprintf(in.Out, "Environment %q is up.\n", in.Name)
	}
	return &StartResult{State: state}, nil
}
