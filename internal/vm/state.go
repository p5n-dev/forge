// Package vm provides the VM runtime layer for FORGE: state persistence,
// state-machine validation, and a thin wrapper around the vfkit subprocess.
//
// All higher-level commands (`forge env create`, `start`, `stop`, etc.) are
// expected to compose the primitives in this package.
package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status is the lifecycle status of a VM environment.
//
// It is a string type so the on-disk JSON representation is human-readable.
type Status string

// Lifecycle statuses, mirroring the diagram in docs/spec.md section 6.
const (
	StatusCreating  Status = "creating"
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusStopping  Status = "stopping"
	StatusStopped   Status = "stopped"
	StatusCrashed   Status = "crashed"
	StatusDestroyed Status = "destroyed"
)

// State is the persisted record for a single environment.
//
// It is serialised to `~/.forge/envs/<name>/state.json`. The schema is
// stable: external tooling and the CLI both depend on the JSON field
// names listed below.
type State struct {
	Name         string    `json:"name"`
	Status       Status    `json:"status"`
	IP           string    `json:"ip"`
	MAC          string    `json:"mac"`
	PID          int       `json:"pid"`
	CreatedAt    time.Time `json:"created_at"`
	ImageVersion string    `json:"image_version"`
	CPUs         int       `json:"cpus"`
	Memory       int       `json:"memory"`
	Disk         int       `json:"disk"`
}

// stateFileName is the basename of the per-env state file.
const stateFileName = "state.json"

// EnvDir returns the absolute path to the per-env directory under
// `~/.forge/envs/<name>`. The leading `~` is expanded to the current
// user's home directory; if it cannot be determined, the literal `~`
// path is returned (callers will see a downstream filesystem error).
func EnvDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("~", ".forge", "envs", name)
	}
	return filepath.Join(home, ".forge", "envs", name)
}

// LoadState reads `state.json` from envDir and returns the parsed State.
func LoadState(envDir string) (*State, error) {
	path := filepath.Join(envDir, stateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the state atomically to `<envDir>/state.json`.
//
// The file is written to `state.json.tmp` first and then renamed into
// place, so a concurrent reader either sees the previous version or
// the new one — never a torn write.
func (s *State) Save(envDir string) error {
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return fmt.Errorf("creating env dir %s: %w", envDir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling state: %w", err)
	}

	tmpPath := filepath.Join(envDir, stateFileName+".tmp")
	finalPath := filepath.Join(envDir, stateFileName)

	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Best-effort cleanup of the tmp file on rename failure.
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming %s -> %s: %w", tmpPath, finalPath, err)
	}

	return nil
}

// allowedTransitions encodes the lifecycle state machine (spec §6).
//
//	creating  -> starting | crashed | destroyed
//	starting  -> running  | stopped | crashed | destroyed
//	running   -> stopping | crashed
//	stopping  -> stopped  | crashed
//	stopped   -> starting | destroyed
//	crashed   -> starting | stopped  | destroyed
//	destroyed -> (terminal)
//
// `starting -> stopped` and `crashed -> stopped` exist for the
// `forge env stop --force` recovery path: a half-booted or
// already-dead env should be reachable from a single command without
// having to destroy + recreate.
var allowedTransitions = map[Status]map[Status]struct{}{
	StatusCreating: {
		StatusStarting:  {},
		StatusCrashed:   {},
		StatusDestroyed: {},
	},
	StatusStarting: {
		StatusRunning:   {},
		StatusStopped:   {},
		StatusCrashed:   {},
		StatusDestroyed: {},
	},
	StatusRunning: {
		StatusStopping: {},
		StatusCrashed:  {},
	},
	StatusStopping: {
		StatusStopped: {},
		StatusCrashed: {},
	},
	StatusStopped: {
		StatusStarting:  {},
		StatusDestroyed: {},
	},
	StatusCrashed: {
		StatusStarting:  {},
		StatusStopped:   {},
		StatusDestroyed: {},
	},
	StatusDestroyed: {},
}

// Transition mutates s.Status to target if and only if the move is
// allowed by the lifecycle state machine. On rejection s.Status is
// left unchanged and a descriptive error is returned.
func (s *State) Transition(target Status) error {
	allowed, ok := allowedTransitions[s.Status]
	if !ok {
		return fmt.Errorf("vm: unknown source status %q", s.Status)
	}
	if _, ok := allowed[target]; !ok {
		return fmt.Errorf("vm: invalid transition %s -> %s", s.Status, target)
	}
	s.Status = target
	return nil
}
