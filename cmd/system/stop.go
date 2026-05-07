package system

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/config"
	"github.com/p5n-dev/forge/internal/forgejo"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop FORGE system services (Forgejo)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStop(cmd.Context(), os.Stdout)
	},
}

func runStop(ctx context.Context, out io.Writer) error {
	configPath, err := globalConfigPath()
	if err != nil {
		return err
	}

	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	if cfg.Forgejo.URL != "" {
		_, _ = fmt.Fprintf(out, "Using external Forgejo at %s. Nothing to stop.\n", cfg.Forgejo.URL)
		return nil
	}

	mgr := forgejo.NewManager(forgejo.Options{})
	if err := mgr.Stop(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Forgejo stopped")
	return nil
}
