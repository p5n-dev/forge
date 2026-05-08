package env

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/config"
	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/forgejo"
	"github.com/p5n-dev/forge/internal/vm"
)

var (
	destroyFlagForce        bool
	destroyFlagPurgeForgejo bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy [name]",
	Short: "Stop the VM and delete all FORGE environment state",
	Long: `Stops the VM (if running) and deletes the env directory and every artefact
it contains: disk image, SSH keys, cloud-init ISO, state file.

By default Forgejo state is preserved — the per-env Forgejo user and its
'workspace' repository remain so review history is not lost. Pass
--purge-forgejo to also delete the Forgejo user and all of its repos.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		deps := envpkg.DestroyDeps{Runner: vm.NewVfkitRunner()}

		if destroyFlagPurgeForgejo {
			client, err := buildForgejoClient()
			if err != nil {
				return fmt.Errorf("--purge-forgejo: %w", err)
			}
			deps.Forgejo = client
		}

		_, err := envpkg.Destroy(cmd.Context(), envpkg.DestroyInput{
			Name:         args[0],
			EnvBaseDir:   envsBaseDir,
			Force:        destroyFlagForce,
			PurgeForgejo: destroyFlagPurgeForgejo,
			In:           cmd.InOrStdin(),
			Out:          cmd.OutOrStdout(),
		}, deps)
		return err
	},
}

func init() {
	destroyCmd.Flags().BoolVar(&destroyFlagForce, "force", false, "skip the interactive confirmation prompt")
	destroyCmd.Flags().BoolVar(&destroyFlagPurgeForgejo, "purge-forgejo", false,
		"also delete the per-env Forgejo user and its repos (default: keep, preserves review history)")
	Cmd.AddCommand(destroyCmd)
}

// buildForgejoClient loads global config and returns a Forgejo APIClient
// pointed at whichever Forgejo this host is configured for (external URL
// or the FORGE-managed local container). Used by destroy when
// --purge-forgejo is set.
func buildForgejoClient() (*forgejo.APIClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("looking up home directory: %w", err)
	}
	configPath := filepath.Join(home, ".forge", "config.yaml")
	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	apiBase, _, _ := resolveForgejo(cfg)
	user := cfg.Forgejo.AdminUser
	if user == "" {
		user = "forge"
	}
	token := cfg.Forgejo.AdminToken
	if cfg.Forgejo.URL != "" {
		token = cfg.Forgejo.Token
	}
	if token == "" {
		return nil, fmt.Errorf("no Forgejo admin token configured. " +
			"Either set forgejo.url + forgejo.token in ~/.forge/config.yaml " +
			"(token must have admin scope), or run 'forge system start' first")
	}
	return forgejo.NewAPIClient(apiBase, user, token), nil
}
