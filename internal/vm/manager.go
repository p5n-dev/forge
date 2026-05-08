package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manager ties persistent State to a Runner. It is the entry point
// other packages use to ask "is this VM healthy?" or to record an
// explicit lifecycle event.
//
// Concurrency: Manager is not safe for concurrent use on the same env
// name. Higher layers serialise per-env operations (one user, one CLI
// invocation at a time).
type Manager struct {
	runner  Runner
	baseDir string
}

// NewManager wires a Runner to a base directory (typically
// `~/.forge/envs`). A leading `~` in baseDir is expanded to the
// current user's home directory.
func NewManager(runner Runner, baseDir string) *Manager {
	return &Manager{runner: runner, baseDir: expandHome(baseDir)}
}

// envDir returns the absolute per-env directory under m.baseDir.
func (m *Manager) envDir(name string) string {
	return filepath.Join(m.baseDir, name)
}

// Status returns the live status for an environment.
//
// Lazy stale-PID detection (spec §6): if the persisted state says
// `running` or `starting` but the runner reports the PID is dead, the
// status is rewritten to `crashed` on disk and `crashed` is returned.
//
// Other statuses (creating, stopping, stopped, crashed, destroyed) are
// returned as-is — those represent intentional state and don't need a
// liveness probe.
func (m *Manager) Status(name string) (Status, error) {
	envDir := m.envDir(name)
	state, err := LoadState(envDir)
	if err != nil {
		return "", err
	}

	if !needsLivenessCheck(state.Status) {
		return state.Status, nil
	}

	alive, err := m.runner.IsAlive(envDir)
	if err != nil {
		return "", fmt.Errorf("checking liveness for %s: %w", name, err)
	}
	if alive {
		return state.Status, nil
	}

	// Process is gone: record as crashed and persist.
	if err := state.Transition(StatusCrashed); err != nil {
		// Should not happen — both running and starting can transition
		// to crashed — but surface it if the state machine ever changes.
		return "", fmt.Errorf("recording crash for %s: %w", name, err)
	}
	if err := state.Save(envDir); err != nil {
		return "", fmt.Errorf("persisting crashed state for %s: %w", name, err)
	}
	return state.Status, nil
}

// MarkStopping records an in-progress shutdown. Used by `forge env stop`
// once it has dispatched the shutdown command but before it confirms
// the VM has exited.
func (m *Manager) MarkStopping(name string) error {
	return m.transition(name, StatusStopping)
}

// MarkStopped records that the VM has cleanly exited. Used by
// `forge env stop` after Runner.Stop returns.
func (m *Manager) MarkStopped(name string) error {
	return m.transition(name, StatusStopped)
}

// MarkCrashed records that the VM died unexpectedly. Used by callers
// that have out-of-band knowledge of a crash; the lazy detection in
// Status covers the common case.
func (m *Manager) MarkCrashed(name string) error {
	return m.transition(name, StatusCrashed)
}

// transition is the common path: load, transition, save.
func (m *Manager) transition(name string, target Status) error {
	envDir := m.envDir(name)
	state, err := LoadState(envDir)
	if err != nil {
		return err
	}
	if err := state.Transition(target); err != nil {
		return err
	}
	return state.Save(envDir)
}

// needsLivenessCheck reports whether a status implies the VM should
// currently be running, and therefore warrants a runtime liveness probe.
func needsLivenessCheck(s Status) bool {
	return s == StatusRunning || s == StatusStarting
}

// expandHome expands a leading `~/` in path. If the home directory
// can't be determined the original path is returned unchanged — the
// caller will see a downstream filesystem error.
func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
