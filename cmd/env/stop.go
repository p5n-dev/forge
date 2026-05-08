package env

import (
	"github.com/spf13/cobra"

	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

var stopFlagForce bool

var stopCmd = &cobra.Command{
	Use:   "stop [name]",
	Short: "Gracefully stop a FORGE environment",
	Long: `Stop a running FORGE environment by sending SIGTERM to vfkit and
recording the env as stopped.

If the env is stuck in 'starting', 'stopping', or 'crashed' (typically
because the previous command was killed before it finished), pass
--force to clear it: that kills any lingering vfkit subprocess and
flips the status straight to 'stopped' so 'forge env start' works
again.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := envpkg.Stop(cmd.Context(), envpkg.StopInput{
			Name:       args[0],
			EnvBaseDir: envsBaseDir,
			Force:      stopFlagForce,
			Out:        cmd.OutOrStdout(),
		}, envpkg.StopDeps{
			Runner:      vm.NewVfkitRunner(),
			NetRunner:   newExecNetRunner(),
			ProxyRunner: newExecProxyRunner(),
		})
		return err
	},
}

func init() {
	stopCmd.Flags().BoolVarP(&stopFlagForce, "force", "f", false,
		"force-stop an env stuck in starting/stopping/crashed")
	Cmd.AddCommand(stopCmd)
}
