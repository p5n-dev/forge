package system

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/config"
	"github.com/p5n-dev/forge/internal/forgejo"
	"github.com/p5n-dev/forge/internal/image"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show health of FORGE system services",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStatus(cmd.Context(), os.Stdout)
	},
}

// statusReport collects everything `forge system status` needs to render.
// It is a plain struct so renderStatus can be unit-tested without touching the
// filesystem or the network.
type statusReport struct {
	ForgejoURL       string
	ForgejoReachable bool
	ForgejoReason    string

	VfkitInstalled bool
	VfkitVersion   string

	ImageDir    string
	LatestImage string
}

var (
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	failStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	dimStyle  = lipgloss.NewStyle().Faint(true)
)

func runStatus(ctx context.Context, out io.Writer) error {
	configPath, err := globalConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	report := statusReport{}

	// Forgejo
	report.ForgejoURL = cfg.Forgejo.URL
	if report.ForgejoURL == "" {
		port := cfg.Forgejo.Port
		if port == 0 {
			port = forgejo.DefaultPort
		}
		report.ForgejoURL = fmt.Sprintf("http://localhost:%d", port)
	}
	report.ForgejoReachable, report.ForgejoReason = forgejo.Reachable(ctx, report.ForgejoURL)

	// vfkit
	report.VfkitVersion, report.VfkitInstalled = lookupVfkitVersion(ctx, "vfkit")

	// Image cache
	imageDir, err := resolveImageCacheDir(cfg.Image.CacheDir)
	if err != nil {
		return err
	}
	report.ImageDir = imageDir
	latest, err := latestImageInDir(imageDir)
	if err != nil {
		return fmt.Errorf("scanning image cache: %w", err)
	}
	report.LatestImage = latest

	return renderStatus(out, report)
}

func renderStatus(out io.Writer, r statusReport) error {
	const checkmark = "✓" // ✓
	const cross = "✗"     // ✗

	// Forgejo line.
	if r.ForgejoReachable {
		_, _ = fmt.Fprintf(out, "%s Forgejo   %s %s\n",
			okStyle.Render(checkmark),
			r.ForgejoURL,
			dimStyle.Render("("+r.ForgejoReason+")"),
		)
	} else {
		_, _ = fmt.Fprintf(out, "%s Forgejo   %s %s\n",
			failStyle.Render(cross),
			r.ForgejoURL,
			dimStyle.Render("("+r.ForgejoReason+")"),
		)
	}

	// vfkit line.
	if r.VfkitInstalled {
		_, _ = fmt.Fprintf(out, "%s vfkit     %s\n",
			okStyle.Render(checkmark),
			r.VfkitVersion,
		)
	} else {
		_, _ = fmt.Fprintf(out, "%s vfkit     %s\n",
			failStyle.Render(cross),
			"not installed",
		)
	}

	// Image cache line.
	if r.LatestImage != "" {
		_, _ = fmt.Fprintf(out, "%s Images    %s %s\n",
			okStyle.Render(checkmark),
			r.LatestImage,
			dimStyle.Render("("+r.ImageDir+")"),
		)
	} else {
		_, _ = fmt.Fprintf(out, "%s Images    no images cached %s\n",
			failStyle.Render(cross),
			dimStyle.Render("("+r.ImageDir+")"),
		)
	}

	return nil
}

// lookupVfkitVersion runs `<bin> --version` and returns the trimmed output.
// If the binary cannot be found or fails to execute, ok is false.
func lookupVfkitVersion(ctx context.Context, bin string) (string, bool) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", false
	}
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// latestImageInDir returns the version string of the most recent base
// image in dir, or "" if the cache is empty or missing. It delegates to
// image.ListCached so `status` and `image list` stay in lockstep on the
// filename convention (`forge-base-<version>-arm64.img.gz`).
func latestImageInDir(dir string) (string, error) {
	images, err := image.ListCached(dir)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		return "", nil
	}
	return images[0].Version, nil
}

// resolveImageCacheDir expands `~` in the configured image cache dir.
func resolveImageCacheDir(cacheDir string) (string, error) {
	if cacheDir == "" {
		cacheDir = "~/.forge/images"
	}
	if strings.HasPrefix(cacheDir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("looking up home directory: %w", err)
		}
		cacheDir = filepath.Join(home, strings.TrimPrefix(cacheDir, "~"))
	}
	return cacheDir, nil
}
