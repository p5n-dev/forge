package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/image"
)

// Styles. Kept module-local so the command file is self-contained; if more
// commands need them later we can promote to a shared internal/ui package.
var (
	infoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

var pullCmd = &cobra.Command{
	Use:   "pull [version]",
	Short: "Download a FORGE base image",
	Long: `Downloads a FORGE base image from the configured ImageSource (GitHub Releases
by default) into ~/.forge/images/.

Without arguments, the latest release is pulled. Pass a version (e.g. v0.1.0) to
pull a specific release. The accompanying SBOM is downloaded alongside the
image, and the SHA256 checksum is verified before the file is considered valid.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPull,
}

func runPull(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
	defer cancel()

	cacheDir, err := resolveCacheDir()
	if err != nil {
		return err
	}

	src := image.NewGitHubReleasesSource("", "")

	version := ""
	if len(args) == 1 {
		version = args[0]
	}

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if version == "" {
		printLine(out, infoStyle.Render("Resolving latest release..."))
		latest, err := src.LatestVersion(ctx)
		if err != nil {
			return fmt.Errorf("looking up latest version: %w", err)
		}
		version = latest
	}

	if image.IsCached(cacheDir, version) {
		printLine(out, successStyle.Render(
			fmt.Sprintf("Image %s already cached at %s", version, image.CachePath(cacheDir, version))))
		return nil
	}

	printLine(out, infoStyle.Render(fmt.Sprintf("Pulling %s into %s", version, cacheDir)))
	printLine(out, dimStyle.Render("  - downloading SHA256SUMS"))
	printLine(out, dimStyle.Render("  - downloading "+image.ImageAssetName(version)))
	printLine(out, dimStyle.Render("  - downloading "+image.PublishedSBOMAssetName))
	printLine(out, dimStyle.Render("  - verifying SHA256"))

	if err := src.Pull(ctx, version, cacheDir); err != nil {
		printLine(errOut, errorStyle.Render("pull failed: "+err.Error()))
		return err
	}

	imagePath := filepath.Join(cacheDir, image.ImageAssetName(version))
	info, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("verifying downloaded image: %w", err)
	}

	printLine(out, successStyle.Render(
		fmt.Sprintf("Pulled %s (%s) -> %s", version, humanSize(info.Size()), imagePath)))
	return nil
}

// printLine is a small wrapper around fmt.Fprintln that swallows the error.
// We're writing to a cobra-managed writer; if it fails there's nothing useful
// to do with the error and reporting it would just create noise on top of a
// terminal that's already failing to display things.
func printLine(w io.Writer, s string) {
	_, _ = fmt.Fprintln(w, s)
}

// resolveCacheDir returns the absolute path to the local image cache, creating
// the directory if necessary. Honours the global config's image.cache_dir
// indirectly via the spec default (~/.forge/images); when issue #1 ships
// global config wiring through the root command this should switch to reading
// the loaded config object.
func resolveCacheDir() (string, error) {
	dir, err := image.ExpandPath("~/.forge/images")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}
	return dir, nil
}

// humanSize renders a byte count as a short human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
