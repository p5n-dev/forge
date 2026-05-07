package system

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/config"
)

var disconnectCmd = &cobra.Command{
	Use:   "disconnect",
	Short: "Forget the configured Forgejo connection",
	Long: `Clears the Forgejo block from ~/.forge/config.yaml so that
` + "`forge system start`" + ` can configure a fresh connection (e.g. to
rotate tokens or switch to a different Forgejo instance).

This is config-only — it does not stop any FORGE-managed Forgejo
container. Use ` + "`forge system stop`" + ` for that. If both are needed,
run stop first.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDisconnect(cmd.OutOrStdout())
	},
}

func runDisconnect(out io.Writer) error {
	configPath, err := globalConfigPath()
	if err != nil {
		return err
	}

	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	if cfg.Forgejo == (config.ForgejoConfig{}) {
		_, _ = fmt.Fprintln(out, "No Forgejo connection configured. Nothing to do.")
		return nil
	}

	prevURL := cfg.Forgejo.URL
	prevPort := cfg.Forgejo.Port
	cfg.Forgejo = config.ForgejoConfig{}

	if err := config.SaveGlobal(configPath, cfg); err != nil {
		return fmt.Errorf("persisting config: %w", err)
	}

	switch {
	case prevURL != "":
		_, _ = fmt.Fprintf(out, "Forgot Forgejo connection to %s.\n", prevURL)
	case prevPort != 0:
		_, _ = fmt.Fprintf(out, "Forgot FORGE-managed Forgejo on port %d.\n", prevPort)
	default:
		_, _ = fmt.Fprintln(out, "Forgot Forgejo connection.")
	}
	_, _ = fmt.Fprintln(out, "Run `forge system start` to set up a new one.")
	return nil
}
