package env

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/cloudinit"
	"github.com/p5n-dev/forge/internal/config"
	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/progress"
	"github.com/p5n-dev/forge/internal/vm"
)

// startSSHTimeout caps how long `forge env start` blocks waiting for
// in-VM sshd to surface through the vsock-bridged ssh.sock. A warm boot
// of an already-bootstrapped disk is typically done in 10–15s on
// M-series; 90s leaves margin for an unusually slow cloud-init while
// still failing fast on a wedged VM.
const startSSHTimeout = 90 * time.Second

var startCmd = &cobra.Command{
	Use:   "start [name]",
	Short: "Start a stopped or crashed FORGE environment",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEnvStart(cmd.Context(), cmd.OutOrStdout(), args[0])
	},
}

func init() {
	Cmd.AddCommand(startCmd)
}

func runEnvStart(ctx context.Context, out io.Writer, name string) error {
	proj, source, err := config.Discover()
	if err != nil {
		return fmt.Errorf("loading forge.yaml: %w", err)
	}
	logProjectSource(source)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("looking up home directory: %w", err)
	}
	configPath := filepath.Join(home, ".forge", "config.yaml")
	global, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	// Resolve the same Forgejo URL `forge env create` uses, so a fresh
	// cloud-init ISO is consistent with what the env was created with.
	// The clone path mirrors the convention used by EnsureRepo:
	// <user>/<env>.git. With the in-VM socat unit forwarding
	// localhost:<port> over vsock to the host, base/host/VM are all
	// the same string.
	base, proxyTarget, port := resolveForgejo(global)
	user := global.Forgejo.AdminUser
	if user == "" {
		user = "forge"
	}
	var remote string
	if base != "" {
		remote = fmt.Sprintf("%s/%s/%s.git", base, user, name)
	}

	token := global.Forgejo.AdminToken
	if global.Forgejo.URL != "" {
		token = global.Forgejo.Token
	}

	in := envpkg.StartInput{
		Name:               name,
		EnvBaseDir:         envsBaseDir,
		K3sVersion:         proj.Bootstrap.K3s,
		ClaudeVersion:      proj.Bootstrap.ClaudeCode,
		HelmVersion:        proj.Bootstrap.Helm,
		ForgejoRemoteURL:   remote,
		RageShareDir:       resolveRageShareDir(home),
		HostUID:            os.Getuid(),
		ForgejoHostBase:    base,
		ForgejoVsockPort:   port,
		ForgejoUser:        user,
		ForgejoToken:       token,
		ForgejoProxyTarget: proxyTarget,
		Out:                out,
	}

	stdout, _ := out.(*os.File)
	if stdout == nil {
		stdout = os.Stdout
	}

	deps := envpkg.StartDeps{
		Runner:      vm.NewVfkitRunner(),
		NetRunner:   newExecNetRunner(),
		ProxyRunner: newExecProxyRunner(),
		WriteISO:    cloudinit.WriteISO,
		WaitForSSH: func(ctx context.Context, sshSockPath string) error {
			return envpkg.WaitForSSH(ctx, sshSockPath, startSSHTimeout)
		},
		Progress: progress.Auto(stdout),
	}

	_, err = envpkg.Start(ctx, in, deps)
	return err
}
