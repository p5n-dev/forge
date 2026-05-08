package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/p5n-dev/forge/internal/config"
	"github.com/p5n-dev/forge/internal/forgejo"
)

// adminPasswordEnv is honoured for non-interactive runs (CI, scripts) where
// nothing can answer a TTY prompt. The matching --admin-user flag covers the
// username side.
const adminPasswordEnv = "FORGE_ADMIN_PASSWORD"

// portScanRange caps how many ports findAvailablePort probes from the
// preferred starting point before giving up.
const portScanRange = 50

// cliTokenName is the Forgejo API-token name FORGE provisions for itself.
// Stable name so re-running `forge system start` against an existing
// Forgejo replaces the old token cleanly.
const cliTokenName = "forge-cli"

var (
	startFlagAdminUser  string
	startFlagMode       string
	startFlagForgejoURL string
	startFlagPort       int
	startFlagForce      bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Set up FORGE's Forgejo connection (existing or fresh)",
	Long: `Configures Forgejo for FORGE. Two modes:

  existing — point FORGE at an already-running Forgejo (e.g. CAGE's).
             Prompts for URL + admin credentials, verifies them, and
             generates an API token FORGE will use for admin work. No
             Docker container is started in this mode.

  new      — start a brand-new FORGE-managed Forgejo Docker container.
             Probes for a free port (preferring 3000), prompts for
             admin credentials, creates the admin user, and generates
             an API token.

Without --mode, you'll be asked which to use.

For non-interactive runs (CI etc.), pass --mode plus the relevant flags
and set FORGE_ADMIN_PASSWORD.

When forgejo.url is already set in ~/.forge/config.yaml, this command
is a no-op — FORGE uses the configured external instance directly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStart(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

func init() {
	startCmd.Flags().StringVar(&startFlagMode, "mode", "",
		"setup mode: 'existing' or 'new' (default: prompt)")
	startCmd.Flags().StringVar(&startFlagForgejoURL, "forgejo-url", "",
		"existing Forgejo URL (only with --mode existing)")
	startCmd.Flags().StringVar(&startFlagAdminUser, "admin-user", "",
		"admin username (default: prompt or 'forge')")
	startCmd.Flags().IntVar(&startFlagPort, "port", 0,
		"explicit port for --mode new (default: probe from 3000 upward)")
	startCmd.Flags().BoolVar(&startFlagForce, "force", false,
		"re-run setup even if Forgejo is already configured (rotates the API token)")
}

func runStart(ctx context.Context, in io.Reader, out io.Writer) error {
	configPath, err := globalConfigPath()
	if err != nil {
		return err
	}

	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	// Already pointing at an external Forgejo — nothing to do, unless
	// --force was passed to rotate the token or switch instances.
	if cfg.Forgejo.URL != "" && !startFlagForce {
		_, _ = fmt.Fprintf(out, "Using external Forgejo at %s. Nothing to start.\n", cfg.Forgejo.URL)
		_, _ = fmt.Fprintln(out, "Pass --force to reconfigure, or run `forge system disconnect` first.")
		return nil
	}
	if startFlagForce && cfg.Forgejo.URL != "" {
		_, _ = fmt.Fprintf(out, "Reconfiguring (current Forgejo: %s).\n\n", cfg.Forgejo.URL)
		cfg.Forgejo = config.ForgejoConfig{}
	}

	mode, err := resolveMode(in, out, startFlagMode)
	if err != nil {
		return err
	}

	switch mode {
	case "existing":
		return runStartExisting(ctx, in, out, &cfg, configPath)
	case "new":
		return runStartNew(ctx, in, out, &cfg, configPath)
	default:
		return fmt.Errorf("unknown mode %q (expected 'existing' or 'new')", mode)
	}
}

// runStartExisting wires FORGE up to an already-running Forgejo. Verifies
// admin credentials work, provisions a CLI API token, and persists the
// URL + token to global config.
func runStartExisting(ctx context.Context, in io.Reader, out io.Writer, cfg *config.GlobalConfig, configPath string) error {
	url, err := resolveExistingURL(in, out, startFlagForgejoURL)
	if err != nil {
		return err
	}

	user, password, err := resolveAdminUserAndPassword(in, out, startFlagAdminUser)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "\nVerifying admin login at %s...\n", url)
	canonicalUser, err := forgejo.VerifyAdminLogin(ctx, url, user, password)
	if err != nil {
		return err
	}
	if canonicalUser != user {
		_, _ = fmt.Fprintf(out, "Resolved login %q to canonical username %q.\n", user, canonicalUser)
	}

	_, _ = fmt.Fprintf(out, "Generating API token (replacing any existing %q token)...\n", cliTokenName)
	token, err := forgejo.EnsureCLIToken(ctx, url, canonicalUser, password, cliTokenName)
	if err != nil {
		return err
	}

	cfg.Forgejo.URL = url
	cfg.Forgejo.Token = token
	cfg.Forgejo.AdminUser = canonicalUser
	cfg.Forgejo.AdminToken = token
	if err := config.SaveGlobal(configPath, *cfg); err != nil {
		return fmt.Errorf("persisting config: %w", err)
	}

	_, _ = fmt.Fprintf(out, "\nFORGE is now configured to use the existing Forgejo at %s.\n", url)
	_, _ = fmt.Fprintf(out, "  Admin user: %s\n", canonicalUser)
	_, _ = fmt.Fprintf(out, "  Token saved to %s\n", configPath)
	return nil
}

// runStartNew is the previous "spin up a container" flow with port
// auto-detection layered on top.
func runStartNew(ctx context.Context, in io.Reader, out io.Writer, cfg *config.GlobalConfig, configPath string) error {
	dataDir, err := defaultDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating forgejo data dir: %w", err)
	}

	port, err := resolvePort(out)
	if err != nil {
		return err
	}

	mgr := forgejo.NewManager(forgejo.Options{
		DataDir: dataDir,
		Port:    port,
	})

	running, err := mgr.IsRunning(ctx)
	if err != nil {
		return fmt.Errorf("checking forgejo state: %w", err)
	}
	if running {
		_, _ = fmt.Fprintf(out, "Forgejo is already running at %s\n", mgr.URL())
		return nil
	}

	user, password, err := resolveAdminUserAndPassword(in, out, startFlagAdminUser)
	if err != nil {
		return err
	}

	log.Info().Int("port", port).Str("admin", user).Msg("starting forgejo")
	creds, err := mgr.Start(ctx, forgejo.AdminCredentials{Username: user, Password: password})
	if err != nil {
		return err
	}

	cfg.Forgejo.Port = port
	cfg.Forgejo.AdminUser = creds.Username
	cfg.Forgejo.AdminToken = creds.Token
	if err := config.SaveGlobal(configPath, *cfg); err != nil {
		return fmt.Errorf("persisting admin credentials: %w", err)
	}

	_, _ = fmt.Fprintf(out, "\nForgejo running at %s\n", mgr.URL())
	_, _ = fmt.Fprintf(out, "  Admin user: %s\n", creds.Username)
	_, _ = fmt.Fprintf(out, "  API token:  saved to %s\n", configPath)
	_, _ = fmt.Fprintf(out, "  Web UI:     log in with the password you just entered\n")
	return nil
}

// resolveMode returns "existing" or "new", taking the flag if set,
// otherwise prompting via in/out. Any other value is an error.
func resolveMode(in io.Reader, out io.Writer, flag string) (string, error) {
	if flag != "" {
		switch flag {
		case "existing", "new":
			return flag, nil
		default:
			return "", fmt.Errorf("invalid --mode %q (expected 'existing' or 'new')", flag)
		}
	}
	if !isTerminal(in) {
		return "", errors.New("--mode is required when running non-interactively")
	}

	_, _ = fmt.Fprintln(out, "How would you like to set up Forgejo?")
	_, _ = fmt.Fprintln(out, "  [1] Use an existing Forgejo instance (e.g. one CAGE is already running)")
	_, _ = fmt.Fprintln(out, "  [2] Start a new FORGE-managed Forgejo container")

	r := bufio.NewReader(in)
	for {
		_, _ = fmt.Fprint(out, "Choose [1/2]: ")
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		switch strings.TrimSpace(line) {
		case "1", "existing":
			return "existing", nil
		case "2", "new":
			return "new", nil
		}
	}
}

func resolveExistingURL(in io.Reader, out io.Writer, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if !isTerminal(in) {
		return "", errors.New("--forgejo-url is required with --mode existing in non-interactive runs")
	}
	return promptDefault(in, out, "Forgejo URL", "http://localhost:4000")
}

// resolvePort honours the explicit --port flag, otherwise scans for a
// free port starting at 3000. Confirms the choice with the user when
// the preferred port was taken (and there's a TTY).
func resolvePort(out io.Writer) (int, error) {
	if startFlagPort > 0 {
		return startFlagPort, nil
	}

	const preferred = forgejo.DefaultPort
	if isPortFree(preferred) {
		return preferred, nil
	}

	free, err := findAvailablePort(preferred, portScanRange)
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(out, "Port %d is busy; using %d instead.\n", preferred, free)
	return free, nil
}

// resolveAdminUserAndPassword prompts (or reads from flags / env) for
// the admin user and password used in BOTH paths: created in `new` mode,
// authenticated against in `existing` mode.
func resolveAdminUserAndPassword(in io.Reader, out io.Writer, flagUser string) (user, password string, err error) {
	user = flagUser

	if envPwd := os.Getenv(adminPasswordEnv); envPwd != "" {
		if user == "" {
			user = "forge"
		}
		return user, envPwd, nil
	}

	if !isTerminal(in) {
		return "", "", fmt.Errorf(
			"no terminal available and %s is not set; either run interactively or set %s and pass --admin-user",
			adminPasswordEnv, adminPasswordEnv)
	}

	if user == "" {
		got, err := promptDefault(in, out, "Admin username", "forge")
		if err != nil {
			return "", "", err
		}
		user = got
	}

	pwd1, err := promptPassword(out, "Admin password")
	if err != nil {
		return "", "", err
	}
	pwd2, err := promptPassword(out, "Confirm admin password")
	if err != nil {
		return "", "", err
	}
	if pwd1 != pwd2 {
		return "", "", errors.New("passwords do not match")
	}
	if pwd1 == "" {
		return "", "", errors.New("admin password cannot be empty")
	}
	return user, pwd1, nil
}

func promptDefault(in io.Reader, out io.Writer, prompt, def string) (string, error) {
	_, _ = fmt.Fprintf(out, "%s [%s]: ", prompt, def)
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptPassword(out io.Writer, prompt string) (string, error) {
	_, _ = fmt.Fprintf(out, "%s: ", prompt)
	pwd, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(pwd), nil
}

// isTerminal reports whether r is the same file descriptor as the
// process's controlling terminal. Used to decide whether interactive
// prompts make sense.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// globalConfigPath returns the path FORGE uses for the global config file.
func globalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("looking up home directory: %w", err)
	}
	return filepath.Join(home, ".forge", "config.yaml"), nil
}

// defaultDataDir returns the default Forgejo data directory.
func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("looking up home directory: %w", err)
	}
	return filepath.Join(home, ".forge", "forgejo"), nil
}
