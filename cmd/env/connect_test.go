package env_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	envcmd "github.com/p5n-dev/forge/cmd/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// TestBuildSSHArgs_Default verifies the argv produced for the default
// (no --bash) connect path: SSH to forge@<envname> with the per-env
// private key, the per-env known_hosts, a ProxyCommand routing through
// the vsock-bridged Unix socket, and immediately exec `rage` inside the
// VM.
func TestBuildSSHArgs_Default(t *testing.T) {
	state := &vm.State{Name: "demo", IP: "192.168.127.42"}
	envDir := "/home/user/.forge/envs/demo"

	args := envcmd.BuildSSHArgsForTest(state, envDir, false)

	// argv[0] must be the ssh binary so syscall.Exec passes it as
	// argv[0] to the kernel; the helper sets it to "ssh" by default.
	require.NotEmpty(t, args)
	assert.Equal(t, "ssh", args[0])

	// Identity key flag points at the per-env private key.
	assertContainsPair(t, args, "-i", envDir+"/id_ed25519")

	// Per-env known_hosts under the env dir, not the user's HOME.
	assertContainsPair(t, args, "-o", "UserKnownHostsFile="+envDir+"/known_hosts")

	// Strict host key checking auto-accepts on first connect.
	assertContainsPair(t, args, "-o", "StrictHostKeyChecking=accept-new")

	// ProxyCommand uses the vsock-bridged Unix socket — this is what
	// makes host→VM SSH work without any IP routing.
	assertContainsPair(t, args, "-o", "ProxyCommand=nc -U '"+envDir+"/ssh.sock'")

	// -t forces PTY allocation. Without it, the remote `bash -l` /
	// `rage` runs without a controlling terminal and looks frozen —
	// bash silently switches to non-interactive mode and reads stdin
	// forever.
	assert.Contains(t, args, "-t")

	// Destination is forge@<envname>, NOT the IP. The IP is irrelevant
	// once ProxyCommand owns connection setup; ssh just needs a stable
	// known_hosts key.
	assert.Contains(t, args, "forge@demo")
	assert.NotContains(t, args, "forge@192.168.127.42",
		"IP-based destination must be gone — it ignored ProxyCommand and timed out under VPN")

	// Default remote command: login bash that sources /etc/profile.d
	// (KUBECONFIG export; the claude binary itself lives at
	// /usr/local/bin/claude via a bootstrap-time symlink), cd's into
	// the virtio-fs workspace mount (so Claude launches inside the
	// project not $HOME), and exec's rage. Passed as ONE arg with
	// internal single-quotes — see connect.go for why the three-arg
	// form collapses on the wire and silently exits.
	assertRemoteCommand(t, args, "forge@demo",
		[]string{"bash -l -c 'cd /home/forge/workspace && exec rage'"})
}

// TestBuildSSHArgs_Bash verifies the --bash path drops to a login bash
// shell on the remote side.
func TestBuildSSHArgs_Bash(t *testing.T) {
	state := &vm.State{Name: "demo", IP: "10.0.0.5"}
	envDir := "/tmp/envs/demo"

	args := envcmd.BuildSSHArgsForTest(state, envDir, true)

	assertContainsPair(t, args, "-i", envDir+"/id_ed25519")
	assert.Contains(t, args, "forge@demo")

	// --bash drops into an interactive login-then-interactive bash,
	// chdir'd into the workspace mount. Same one-arg + internal-quotes
	// shape as the rage path so the cd survives the remote tokenizer.
	assertRemoteCommand(t, args, "forge@demo",
		[]string{"bash -l -c 'cd /home/forge/workspace && exec bash -i'"})
}

// TestBuildSSHArgs_QuotesPathWithSpaces ensures the ProxyCommand value
// survives macOS home directories like "/Users/Some User/" which would
// otherwise be split into two shell tokens.
func TestBuildSSHArgs_QuotesPathWithSpaces(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/Users/Some User/.forge/envs/demo"

	args := envcmd.BuildSSHArgsForTest(state, envDir, false)
	assertContainsPair(t, args, "-o",
		"ProxyCommand=nc -U '/Users/Some User/.forge/envs/demo/ssh.sock'")
}

// TestConnect_EnvNotFound asserts a missing env name surfaces a clear
// error instead of crashing on a missing state.json.
func TestConnect_EnvNotFound(t *testing.T) {
	base := t.TempDir()
	restore := envcmd.SetEnvsBaseDirForTest(base)
	t.Cleanup(restore)

	// Stub execSSH so a successful path can't accidentally exec.
	restoreExec := envcmd.SetExecSSHForTest(func(_ string, _ []string, _ []string) error {
		return errors.New("execSSH must not be called when env is missing")
	})
	t.Cleanup(restoreExec)

	out := &bytes.Buffer{}
	envcmd.Cmd.SetOut(out)
	envcmd.Cmd.SetErr(out)
	envcmd.Cmd.SetArgs([]string{"connect", "ghost"})

	err := envcmd.Cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost",
		"error must mention the env name so the user knows which env wasn't found")
}

// TestConnect_EnvNotRunning asserts an env that exists but is not in
// `running` state returns a friendly message pointing at `forge env start`.
func TestConnect_EnvNotRunning(t *testing.T) {
	base := t.TempDir()
	restore := envcmd.SetEnvsBaseDirForTest(base)
	t.Cleanup(restore)

	// Persisted state: the env exists but is stopped. No PID file so the
	// liveness probe is skipped (Manager.Status returns the persisted
	// status verbatim for non-running statuses).
	writeStateJSON(t, base, "foo", vm.State{
		Name:   "foo",
		Status: vm.StatusStopped,
		IP:     "192.168.127.10",
	})

	called := false
	restoreExec := envcmd.SetExecSSHForTest(func(_ string, _ []string, _ []string) error {
		called = true
		return nil
	})
	t.Cleanup(restoreExec)

	out := &bytes.Buffer{}
	envcmd.Cmd.SetOut(out)
	envcmd.Cmd.SetErr(out)
	envcmd.Cmd.SetArgs([]string{"connect", "foo"})

	err := envcmd.Cmd.Execute()
	require.Error(t, err)
	assert.False(t, called, "execSSH must not be called when env is not running")

	msg := err.Error()
	assert.Contains(t, msg, "foo", "error must name the env")
	assert.Contains(t, msg, "stopped", "error must surface the actual status")
	assert.Contains(t, msg, "forge env start",
		"error must point the user at how to bring the env back up")
}

// assertContainsPair fails the test if `flag value` does not appear as
// two consecutive tokens in args. This catches cases where a flag and
// its value end up split across unrelated positions (a bug ssh would
// silently misinterpret).
func assertContainsPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("expected consecutive args %q %q in %v", flag, value, args)
}

// assertRemoteCommand fails the test unless the given tokens appear in
// order after the destination ("forge@<ip>"). Tokens between the
// destination and the first remote-command token are ignored so the
// implementation is free to add or reorder ssh options.
func assertRemoteCommand(t *testing.T, args []string, dest string, want []string) {
	t.Helper()

	// Locate the destination.
	idx := -1
	for i, a := range args {
		if a == dest {
			idx = i
			break
		}
	}
	require.NotEqualf(t, -1, idx, "destination %q not in %v", dest, args)

	tail := args[idx+1:]
	if len(tail) < len(want) {
		t.Fatalf("expected remote command tokens %v after %q, got %v",
			want, dest, tail)
	}
	got := tail[:len(want)]
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("remote command mismatch at %d: want %q got %q (full tail %v)",
				i, w, got[i], tail)
		}
	}
	// Sanity: nothing after the remote command. The connect helper
	// shouldn't sneak extra tokens past the command boundary.
	if len(tail) > len(want) {
		t.Fatalf("unexpected trailing args after remote command: %v",
			strings.Join(tail[len(want):], " "))
	}
}
