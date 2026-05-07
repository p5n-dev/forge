package env

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

// bootstrapLogPath is where the in-VM forge-bootstrap script logs.
// Hard-coded here AND in internal/cloudinit/userdata.go's
// forge-bootstrap script. Two references but it's a path-as-string;
// not worth a shared constant just to dodge the duplication.
const bootstrapLogPath = "/var/log/forge-bootstrap.log"

var (
	logsFlagFollow bool
	logsFlagLines  int
)

var logsCmd = &cobra.Command{
	Use:   "logs [name]",
	Short: "Tail the in-VM bootstrap log",
	Long: `Tail /var/log/forge-bootstrap.log inside a running or
starting env via the vsock-bridged SSH socket.

Useful for watching cloud-init bootstrap progress while 'forge env
create' is still spinning, without the SSH-via-nc-via-ProxyCommand
dance. Also handy for reviewing what an env did during its boot
after the fact.

Examples:

    forge env logs myproj          # last 100 lines, exit
    forge env logs myproj -f       # follow live (Ctrl-C to stop)
    forge env logs myproj -n 500   # last 500 lines
`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEnvLogs(args[0], logsFlagFollow, logsFlagLines)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFlagFollow, "follow", "f", false,
		"follow the log (live updates as new lines appear)")
	logsCmd.Flags().IntVarP(&logsFlagLines, "lines", "n", 100,
		"number of lines to print before following")
	Cmd.AddCommand(logsCmd)
}

func runEnvLogs(name string, follow bool, lines int) error {
	envDir := filepath.Join(envsBaseDir, name)
	if _, err := os.Stat(envDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("env %q not found (looked in %s)", name, envDir)
		}
		return fmt.Errorf("checking env %q: %w", name, err)
	}

	manager := vm.NewManager(vm.NewVfkitRunner(), envsBaseDir)
	status, err := manager.Status(name)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}
	// Same reachability rule as 'forge env connect': only running or
	// starting envs have a live sshd. Crashed/stopped envs lose ssh
	// when vfkit is reaped, and creating-pre-vfkit can't be reached
	// either (vsock socket isn't bound yet).
	if status != vm.StatusRunning && status != vm.StatusStarting {
		return fmt.Errorf(
			"env %q is %s — logs requires running or starting (start it with 'forge env start %s')",
			name, status, name,
		)
	}

	state, err := vm.LoadState(envDir)
	if err != nil {
		return fmt.Errorf("loading env %q: %w", name, err)
	}

	sshSock := envpkg.SSHSocketPath(envDir)
	if _, err := os.Stat(sshSock); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(
				"env %q has no ssh.sock at %s — vfkit may not have bound it yet, retry in a few seconds",
				name, sshSock,
			)
		}
		return fmt.Errorf("statting ssh.sock: %w", err)
	}

	args := buildLogsSSHArgs(state, envDir, follow, lines)
	sshPath, err := lookSSHPath()
	if err != nil {
		return err
	}
	args[0] = sshPath
	return execSSH(sshPath, args, os.Environ())
}

// buildLogsSSHArgs returns argv for ssh-tailing the bootstrap log.
//
// The shape mirrors connect.go's buildSSHArgs (same ProxyCommand,
// same key, same destination form), but the remote command is just
// `tail` — no need for a login-shell wrapper since /usr/bin/tail is
// on the default sshd PATH.
//
// PTY allocation (-t) is only set when following: tail line-buffers
// when isatty(stdout) is true, full-buffers otherwise. Without -t and
// without -f, tail prints its lines and exits cleanly — piping/
// redirecting works without CRLF translation. With -f and without
// -t, tail's live output sits in a 4–8 KB stdio buffer and looks
// frozen, so we force the PTY in that case.
func buildLogsSSHArgs(state *vm.State, envDir string, follow bool, lines int) []string {
	keyPath := filepath.Join(envDir, "id_ed25519")
	knownHosts := filepath.Join(envDir, "known_hosts")
	sshSock := envpkg.SSHSocketPath(envDir)

	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "ProxyCommand=nc -U " + shellQuote(sshSock),
	}
	if follow {
		args = append(args, "-t")
	}
	args = append(args, "forge@"+state.Name)

	// `tail -F` follows by name and survives log rotation; `-f` would
	// follow the file descriptor and miss new content if the file
	// gets truncated/recreated. Cheap correctness win.
	tailCmd := fmt.Sprintf("tail -n %d", lines)
	if follow {
		tailCmd = fmt.Sprintf("tail -F -n %d", lines)
	}
	tailCmd += " " + bootstrapLogPath
	args = append(args, tailCmd)

	return args
}

// BuildLogsSSHArgsForTest exposes buildLogsSSHArgs for tests in the
// _test package next door.
func BuildLogsSSHArgsForTest(state *vm.State, envDir string, follow bool, lines int) []string {
	return buildLogsSSHArgs(state, envDir, follow, lines)
}
