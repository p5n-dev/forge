package env

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/p5n-dev/forge/internal/vm"
)

// StopInput is the user-facing configuration for `forge env stop`.
type StopInput struct {
	Name       string
	EnvBaseDir string // ~/.forge/envs (already expanded)
	// Force allows stopping an env stuck in `starting`, `stopping`, or
	// `crashed` — typically because the previous start/stop process was
	// killed before it could complete. Without this flag Stop only
	// accepts `running` (and is a no-op on `stopped`).
	Force bool
	// Out is where Stop prints user-visible messages (e.g. "already stopped").
	// Tests pass a bytes.Buffer; the cobra wrapper passes os.Stdout.
	Out io.Writer
}

// StopDeps groups the side-effecting collaborators of Stop.
type StopDeps struct {
	Runner      vm.Runner
	NetRunner   NetRunner
	ProxyRunner ProxyRunner
}

// StopResult is what Stop returns on success.
type StopResult struct {
	State *vm.State
}

// Stop sends a graceful shutdown to the VM and updates state to `stopped`.
//
// It is idempotent on already-stopped envs: if the env is already in
// `stopped`, Stop returns nil after printing a friendly message. Other
// non-running statuses (creating, starting, stopping, crashed) are
// rejected — the operator should use start/destroy or wait for the
// in-progress transition to complete.
//
// Disk image, SSH keys, cloud-init ISO, and state file are all preserved.
func Stop(ctx context.Context, in StopInput, deps StopDeps) (*StopResult, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("env name is required")
	}
	if in.EnvBaseDir == "" {
		return nil, fmt.Errorf("env base dir is required")
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

	if state.Status == vm.StatusStopped {
		if in.Out != nil {
			_, _ = fmt.Fprintf(in.Out, "Environment %q is already stopped.\n", in.Name)
		}
		return &StopResult{State: state}, nil
	}

	switch state.Status {
	case vm.StatusRunning:
		// Normal happy-path; fall through to graceful stop below.
	case vm.StatusStarting, vm.StatusStopping, vm.StatusCrashed:
		if !in.Force {
			return nil, fmt.Errorf(
				"env %q is %s — pass --force to stop it anyway (kills any vfkit subprocess and resets state to stopped)",
				in.Name, state.Status)
		}
	default:
		return nil, fmt.Errorf("env %q is %s — cannot stop", in.Name, state.Status)
	}

	// For the running path we record the in-progress `stopping`
	// transition so a parallel `forge env list` sees it. For the
	// forced path we go straight to stopped after telling the runner
	// to clean up — there's no "graceful shutdown" to model when
	// vfkit is already dead or wedged.
	if state.Status == vm.StatusRunning {
		if err := state.Transition(vm.StatusStopping); err != nil {
			return nil, fmt.Errorf("transitioning to stopping: %w", err)
		}
		if err := state.Save(envDir); err != nil {
			return nil, fmt.Errorf("saving state: %w", err)
		}
	}

	if err := deps.Runner.Stop(ctx, envDir); err != nil {
		return nil, fmt.Errorf("stopping vm: %w", err)
	}
	// Reap the host-side sister processes too — their unix sockets
	// would otherwise linger and block the next start. Best-effort:
	// a stale PID file is not worth failing the user-visible stop on.
	if deps.ProxyRunner != nil {
		_ = deps.ProxyRunner.Stop(ctx, envDir)
	}
	if deps.NetRunner != nil {
		_ = deps.NetRunner.Stop(ctx, envDir)
	}

	// {stopping|starting|crashed} -> Stopped.
	if err := state.Transition(vm.StatusStopped); err != nil {
		return nil, fmt.Errorf("transitioning to stopped: %w", err)
	}
	// Clear the runtime-only IP — the VM will get a fresh address on
	// next start. CreatedAt, ImageVersion, MAC, CPUs, Memory, Disk are
	// all preserved.
	state.IP = ""
	if err := state.Save(envDir); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	if in.Out != nil {
		_, _ = fmt.Fprintf(in.Out, "Environment %q stopped.\n", in.Name)
	}
	return &StopResult{State: state}, nil
}
