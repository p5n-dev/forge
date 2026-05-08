package env_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	envcmd "github.com/p5n-dev/forge/cmd/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// TestBuildPlainSSHArgs_NoRemote: with no remote command, argv ends at
// the destination — ssh's default PTY-when-no-cmd behaviour gives the
// user a shell. No -t needed.
func TestBuildPlainSSHArgs_NoRemote(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/home/user/.forge/envs/demo"

	args := envcmd.BuildPlainSSHArgsForTest(state, envDir, nil)

	require.NotEmpty(t, args)
	assert.Equal(t, "ssh", args[0])
	assertContainsPair(t, args, "-i", envDir+"/id_ed25519")
	assertContainsPair(t, args, "-o", "UserKnownHostsFile="+envDir+"/known_hosts")
	assertContainsPair(t, args, "-o", "StrictHostKeyChecking=accept-new")
	assertContainsPair(t, args, "-o", "ProxyCommand=nc -U '"+envDir+"/ssh.sock'")

	// Plain ssh must not force a PTY — ssh's default behaviour
	// (PTY only when there is no remote command) is exactly what we
	// want, and forcing -t would break non-interactive use of this
	// same argv shape.
	assert.NotContains(t, args, "-t")

	// Destination is forge@<envname>, and there's nothing past it
	// when remote is nil.
	assert.Equal(t, "forge@demo", args[len(args)-1])
}

// TestBuildPlainSSHArgs_WithRemote: with a remote command, argv has the
// command appended verbatim after the destination, and still no -t.
func TestBuildPlainSSHArgs_WithRemote(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/home/user/.forge/envs/demo"

	args := envcmd.BuildPlainSSHArgsForTest(state, envDir, []string{"echo", "ok-from-vm"})

	assert.Contains(t, args, "forge@demo")
	assert.NotContains(t, args, "-t",
		"non-interactive remote command must not allocate a PTY")

	// Remote command tokens land in order at the tail of argv.
	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, "echo", args[len(args)-2])
	assert.Equal(t, "ok-from-vm", args[len(args)-1])
}

// TestBuildPlainSSHArgs_QuotesPathWithSpaces: ProxyCommand value
// survives macOS-style "/Users/Some User/" paths intact.
func TestBuildPlainSSHArgs_QuotesPathWithSpaces(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/Users/Some User/.forge/envs/demo"

	args := envcmd.BuildPlainSSHArgsForTest(state, envDir, []string{"echo", "x"})
	assertContainsPair(t, args, "-o",
		"ProxyCommand=nc -U '/Users/Some User/.forge/envs/demo/ssh.sock'")
}

// TestSSH_EnvNotFound: missing env surfaces a clear error and never
// reaches execSSH.
func TestSSH_EnvNotFound(t *testing.T) {
	base := t.TempDir()
	restore := envcmd.SetEnvsBaseDirForTest(base)
	t.Cleanup(restore)

	restoreExec := envcmd.SetExecSSHForTest(func(_ string, _ []string, _ []string) error {
		return errors.New("execSSH must not be called when env is missing")
	})
	t.Cleanup(restoreExec)

	out := &bytes.Buffer{}
	envcmd.Cmd.SetOut(out)
	envcmd.Cmd.SetErr(out)
	envcmd.Cmd.SetArgs([]string{"ssh", "ghost", "--", "echo", "hi"})

	err := envcmd.Cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost",
		"error must mention the env name so the user knows which env wasn't found")
}

// TestSSH_EnvNotRunning: stopped env returns a friendly message
// pointing at `forge env start` and never reaches execSSH.
func TestSSH_EnvNotRunning(t *testing.T) {
	base := t.TempDir()
	restore := envcmd.SetEnvsBaseDirForTest(base)
	t.Cleanup(restore)

	writeStateJSON(t, base, "foo", vm.State{
		Name:   "foo",
		Status: vm.StatusStopped,
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
	envcmd.Cmd.SetArgs([]string{"ssh", "foo", "--", "echo", "hi"})

	err := envcmd.Cmd.Execute()
	require.Error(t, err)
	assert.False(t, called, "execSSH must not be called when env is not running")

	msg := err.Error()
	assert.Contains(t, msg, "foo")
	assert.Contains(t, msg, "stopped")
	assert.Contains(t, msg, "forge env start")
}
