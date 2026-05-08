package env_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	envcmd "github.com/p5n-dev/forge/cmd/env"
	"github.com/p5n-dev/forge/internal/vm"
)

func TestBuildLogsSSHArgs_StaticTail(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/tmp/envs/demo"

	args := envcmd.BuildLogsSSHArgsForTest(state, envDir, false, 100)
	joined := strings.Join(args, " ")

	// Same ssh shape as connect: per-env key + vsock-bridged ProxyCommand.
	assert.Contains(t, joined, "-i /tmp/envs/demo/id_ed25519")
	assert.Contains(t, joined, "ProxyCommand=nc -U '/tmp/envs/demo/ssh.sock'")
	assert.Contains(t, joined, "forge@demo")

	// Static (no -f): no PTY allocation. Piped/redirected output should
	// not pick up CRLF translation.
	assert.NotContains(t, args, "-t",
		"static tail must not allocate a PTY — would corrupt redirected output")

	// Remote command is tail with -n N. -F (follow-by-name) only when
	// following.
	assertRemoteCommand(t, args, "forge@demo",
		[]string{"tail -n 100 /var/log/forge-bootstrap.log"})
}

func TestBuildLogsSSHArgs_FollowTail(t *testing.T) {
	state := &vm.State{Name: "demo"}
	envDir := "/tmp/envs/demo"

	args := envcmd.BuildLogsSSHArgsForTest(state, envDir, true, 50)

	// Follow MUST set -t. Without it, tail full-buffers stdout and the
	// live stream sits invisibly in a 4-8 KB buffer.
	assert.Contains(t, args, "-t",
		"-f mode must allocate a PTY so tail line-buffers")

	// `tail -F` (uppercase) follows by name and survives rotation; `-f`
	// follows by descriptor.
	assertRemoteCommand(t, args, "forge@demo",
		[]string{"tail -F -n 50 /var/log/forge-bootstrap.log"})
}
