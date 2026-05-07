package env

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/p5n-dev/forge/internal/vm"
)

// DestroyInput is the user-facing configuration for `forge env destroy`.
type DestroyInput struct {
	Name       string
	EnvBaseDir string // ~/.forge/envs (already expanded)
	// Force skips the interactive confirmation prompt. Mapped to --force
	// on the cobra command.
	Force bool
	// PurgeForgejo also deletes the per-env Forgejo user (and its repo)
	// after the local env is removed. Default false — keeps review
	// history. Mapped to --purge-forgejo on the cobra command.
	PurgeForgejo bool
	// In is where Destroy reads the user's confirmation when Force is
	// false. The cobra wrapper passes os.Stdin; tests pass a
	// strings.Reader.
	In io.Reader
	// Out is where Destroy prints the prompt and the result. Required
	// when Force is false (we need somewhere to ask).
	Out io.Writer
}

// ForgejoPurger is the optional collaborator destroy uses when
// PurgeForgejo is true. Production code wires the *forgejo.APIClient;
// tests pass a fake.
type ForgejoPurger interface {
	PurgeEnvUser(ctx context.Context, envName string) error
}

// DestroyDeps groups the side-effecting collaborators of Destroy.
type DestroyDeps struct {
	Runner  vm.Runner
	Forgejo ForgejoPurger // may be nil when PurgeForgejo is false
}

// DestroyResult is what Destroy returns. Removed reports whether the env
// dir was actually deleted — false when the user declined the prompt.
// PurgedForgejo reports whether the per-env Forgejo user was deleted.
type DestroyResult struct {
	Removed       bool
	PurgedForgejo bool
}

// Destroy stops the VM (if running) and deletes the env directory and
// every artefact it contains: disk image, SSH keys, cloud-init ISO,
// state file, and any optional files like `known_hosts`.
//
// When Force is false Destroy asks for confirmation by reading a single
// line from `in`. The user must type `y`, `Y`, `yes`, or `Yes`
// (case-insensitive). Any other input cancels the operation and returns
// `{Removed: false}` with a nil error.
func Destroy(ctx context.Context, in DestroyInput, deps DestroyDeps) (*DestroyResult, error) {
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

	if !in.Force {
		confirmed, err := confirmDestroy(in.Name, in.In, in.Out)
		if err != nil {
			return nil, fmt.Errorf("reading confirmation: %w", err)
		}
		if !confirmed {
			if in.Out != nil {
				_, _ = fmt.Fprintln(in.Out, "Aborted.")
			}
			return &DestroyResult{Removed: false}, nil
		}
	}

	// Best-effort load: if state.json is missing or unreadable we still
	// try to remove the directory so a half-created env can be cleaned
	// up. We only call runner.Stop when we have a status that suggests
	// the VM might be live.
	state, err := vm.LoadState(envDir)
	if err == nil && shouldStopBeforeDestroy(state.Status) {
		if err := deps.Runner.Stop(ctx, envDir); err != nil {
			return nil, fmt.Errorf("stopping vm: %w", err)
		}
	}

	if err := os.RemoveAll(envDir); err != nil {
		return nil, fmt.Errorf("removing env dir: %w", err)
	}

	res := &DestroyResult{Removed: true}

	if in.PurgeForgejo {
		if deps.Forgejo == nil {
			return nil, fmt.Errorf("--purge-forgejo set but no Forgejo client configured " +
				"(forgejo.url + forgejo.token must be set in ~/.forge/config.yaml, " +
				"or run 'forge system start' to use the FORGE-managed Forgejo)")
		}
		if err := deps.Forgejo.PurgeEnvUser(ctx, in.Name); err != nil {
			return nil, fmt.Errorf("purging forgejo user: %w", err)
		}
		res.PurgedForgejo = true
	}

	if in.Out != nil {
		if res.PurgedForgejo {
			_, _ = fmt.Fprintf(in.Out, "Environment %q destroyed; Forgejo user purged.\n", in.Name)
		} else {
			_, _ = fmt.Fprintf(in.Out, "Environment %q destroyed. Forgejo state preserved (use --purge-forgejo to delete it too).\n", in.Name)
		}
	}
	return res, nil
}

// confirmDestroy prints the destruction prompt to out and reads a single
// line from in. Returns true if the line is an affirmative answer.
func confirmDestroy(name string, in io.Reader, out io.Writer) (bool, error) {
	if in == nil {
		// No reader means we have no way to ask — treat as decline.
		return false, nil
	}
	if out != nil {
		_, _ = fmt.Fprintf(out, "Destroy environment %q? This will permanently delete its disk and all state. [y/N]: ", name)
	}
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// shouldStopBeforeDestroy reports whether a VM in the given status might
// still have a running vfkit subprocess. Runner.Stop is a no-op on a
// missing PID file, so calling it on a crashed env is safe.
func shouldStopBeforeDestroy(s vm.Status) bool {
	switch s {
	case vm.StatusRunning, vm.StatusStarting, vm.StatusStopping, vm.StatusCrashed:
		return true
	default:
		return false
	}
}
