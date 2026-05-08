package image

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/image"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	cellStyle   = lipgloss.NewStyle()
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List locally cached FORGE base images",
	RunE:  runList,
}

func runList(cmd *cobra.Command, _ []string) error {
	cacheDir, err := image.ExpandPath("~/.forge/images")
	if err != nil {
		return err
	}

	images, err := image.ListCached(cacheDir)
	if err != nil {
		return fmt.Errorf("listing cached images: %w", err)
	}

	if len(images) == 0 {
		printLine(cmd.OutOrStdout(), dimStyle.Render(
			"No images cached. Run `forge image pull` to download the latest base image."))
		return nil
	}

	now := time.Now()
	printLine(cmd.OutOrStdout(), renderTable(images, now))
	return nil
}

// renderTable lays out the cached images as a fixed-width table. We hand-roll
// it rather than pulling in lipgloss/table to keep the dependency surface
// small; lipgloss is already vendored for styling.
func renderTable(images []image.CachedImage, now time.Time) string {
	const (
		colVersion = 24
		colSize    = 12
		colPulled  = 16
	)

	pad := func(s string, n int) string {
		if len(s) >= n {
			return s
		}
		return s + spaces(n-len(s))
	}

	var out string
	out += headerStyle.Render(pad("VERSION", colVersion)+pad("SIZE", colSize)+pad("PULLED", colPulled)) + "\n"
	for _, img := range images {
		row := pad(img.Version, colVersion) +
			pad(humanSize(img.Size), colSize) +
			pad(humanRelative(now.Sub(img.PulledAt)), colPulled)
		out += cellStyle.Render(row) + "\n"
	}
	return out
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// humanRelative renders a duration like "2h ago" / "3d ago".
func humanRelative(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	}
}
