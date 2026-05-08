package env

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

var sshCmd = &cobra.Command{
	Use:   "ssh [name] [-- command [args...]]",
	Short: "SSH into a FORGE environment, optionally running a command",
	Long: `Open an SSH session to the named environment.

Without trailing arguments, opens an interactive shell. With a trailing
command (after '--'), runs the command non-interactively and exits with
the remote command's exit code:

  forge env ssh demo                   # interactive shell
  forge env ssh demo -- echo hello     # run echo, exit when done
  forge env ssh demo -- ls -la /etc    # use -- so flags reach the VM

Connection rides the env's vsock-bridged Unix socket — same path as
'forge env connect' — so it works with a tunnel-all corp VPN connected.
The destination hostname is the env name; IP routing is not used.`,
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSSH(args[0], args[1:])
	},
}

func init() {
	Cmd.AddCommand(sshCmd)
}

// runSSH replaces this process with ssh. When remote is empty, ssh
// opens an interactive shell (ssh's default PTY allocation kicks in).
// When non-empty, ssh runs the command non-interactively and forge
// exits with the remote command's exit code via ssh's exit-code
// passthrough.
func runSSH(name string, remote []string) error {
	envDir := filepath.Join(envsBaseDir, name)

	if _, err := os.Stat(filepath.Join(envDir, "state.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("env %q not found (looked in %s)", name, envDir)
		}
		return fmt.Errorf("checking env %q: %w", name, err)
	}

	manager := vm.NewManager(vm.NewVfkitRunner(), envsBaseDir)
	status, err := manager.Status(name)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}
	if status != vm.StatusRunning && status != vm.StatusStarting {
		return fmt.Errorf(
			"env %q is %s — ssh requires running or starting (start it with 'forge env start %s')",
			name, status, name,
		)
	}

	state, err := vm.LoadState(envDir)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}

	sshSock := envpkg.SSHSocketPath(envDir)
	if _, err := os.Stat(sshSock); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"env %q has no ssh.sock at %s — recreate it (forge env destroy %s && forge env create %s) to enable vsock-bridged SSH",
				name, sshSock, name, name,
			)
		}
		return fmt.Errorf("checking ssh socket: %w", err)
	}

	sshPath, err := lookSSHPath()
	if err != nil {
		return fmt.Errorf("locating ssh: %w", err)
	}

	args := buildPlainSSHArgs(state, envDir, remote)
	args[0] = sshPath

	return execSSH(sshPath, args, os.Environ())
}

// buildPlainSSHArgs assembles the argv for `forge env ssh`. Same
// connection setup as buildSSHArgs in connect.go (key, ProxyCommand,
// hostname), but no -t, no rage/bash wrapping, and no cd to
// /home/forge/workspace. ssh's defaults apply: PTY only when there is
// no remote command, and the remote exit code becomes ssh's exit code.
func buildPlainSSHArgs(state *vm.State, envDir string, remote []string) []string {
	keyPath := filepath.Join(envDir, "id_ed25519")
	knownHosts := filepath.Join(envDir, "known_hosts")
	sshSock := envpkg.SSHSocketPath(envDir)

	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "ProxyCommand=nc -U " + shellQuote(sshSock),
		"forge@" + state.Name,
	}
	args = append(args, remote...)
	return args
}

// BuildPlainSSHArgsForTest exposes buildPlainSSHArgs to the external
// test package. Production code uses buildPlainSSHArgs directly.
func BuildPlainSSHArgsForTest(state *vm.State, envDir string, remote []string) []string {
	return buildPlainSSHArgs(state, envDir, remote)
}
