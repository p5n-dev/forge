package env

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/vm"
)

// envsBaseDir is the directory `forge env list` walks. It defaults to
// `~/.forge/envs`, with `~` resolved at process start. Tests override it
// via SetEnvsBaseDirForTest so they can point at a fabricated layout
// without leaking through the real user's home.
var envsBaseDir = defaultEnvsBaseDir()

func defaultEnvsBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to the literal — downstream filesystem ops will
		// surface a more useful error than panicking here.
		return filepath.Join("~", ".forge", "envs")
	}
	return filepath.Join(home, ".forge", "envs")
}

// SetEnvsBaseDirForTest swaps the base directory used by `forge env list`
// and returns a function that restores the original. Exported only for
// the cmd/env tests; production callers should never touch it.
func SetEnvsBaseDirForTest(dir string) func() {
	prev := envsBaseDir
	envsBaseDir = dir
	return func() { envsBaseDir = prev }
}

// HumanRelativeForTest exposes humanRelative to the external test
// package. Keeping the helper unexported in the production API means we
// don't commit to it as a stable surface.
func HumanRelativeForTest(d time.Duration) string {
	return humanRelative(d)
}

// Lipgloss styles. Colour numbers follow ANSI 256 — they degrade
// gracefully on terminals that don't support full colour.
var (
	listHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	listCellStyle   = lipgloss.NewStyle()
	listDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	statusRunningStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	statusStoppedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	statusCrashedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	statusTransientStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	statusOtherStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List FORGE environments with live status",
	RunE:  runList,
}

// envRow is the rendered form of a single env. Keeping the renderer
// pure-data simplifies the table layout and lets tests reason about
// rows without touching lipgloss internals.
type envRow struct {
	name    string
	status  vm.Status
	ip      string
	cpus    int
	memory  int
	created time.Time
}

func runList(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	rows, err := collectRows(envsBaseDir, errOut)
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		printLine(out, listDimStyle.Render("No environments found."))
		return nil
	}

	// Stable order (alphabetical) so the output is predictable both for
	// users and tests.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	printLine(out, renderEnvTable(rows, time.Now()))
	return nil
}

// collectRows walks baseDir for per-env directories and assembles a
// rendered row for each. Missing or malformed state files are skipped
// (with a stderr warning for the malformed case); a missing baseDir
// returns no rows and no error so the caller prints the friendly empty
// message.
func collectRows(baseDir string, errOut io.Writer) ([]envRow, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", baseDir, err)
	}

	runner := vm.NewVfkitRunner()
	manager := vm.NewManager(runner, baseDir)

	rows := make([]envRow, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		envDir := filepath.Join(baseDir, name)

		if _, statErr := os.Stat(filepath.Join(envDir, "state.json")); statErr != nil {
			// No state.json — not a managed env. Quietly skip.
			continue
		}

		// Manager.Status performs the live PID check and persists the
		// crashed status if appropriate. We then re-load the State to
		// pick up the rest of the fields the table wants.
		status, sErr := manager.Status(name)
		if sErr != nil {
			_, _ = fmt.Fprintf(errOut, "warning: skipping %s: %v\n", name, sErr)
			continue
		}

		state, lErr := vm.LoadState(envDir)
		if lErr != nil {
			_, _ = fmt.Fprintf(errOut, "warning: skipping %s: %v\n", name, lErr)
			continue
		}

		rows = append(rows, envRow{
			name:    name,
			status:  status,
			ip:      state.IP,
			cpus:    state.CPUs,
			memory:  state.Memory,
			created: state.CreatedAt,
		})
	}
	return rows, nil
}

// renderEnvTable lays out the rows as a fixed-width table. We follow the
// same hand-rolled approach as cmd/image/list.go to keep the dependency
// surface small.
func renderEnvTable(rows []envRow, now time.Time) string {
	const (
		colName    = 16
		colStatus  = 10
		colIP      = 18
		colCPUs    = 6
		colMem     = 8
		colCreated = 14
	)

	pad := func(s string, n int) string {
		if len(s) >= n {
			return s
		}
		return s + listSpaces(n-len(s))
	}

	var out string
	out += listHeaderStyle.Render(
		pad("NAME", colName)+
			pad("STATUS", colStatus)+
			pad("IP", colIP)+
			pad("CPUS", colCPUs)+
			pad("MEM", colMem)+
			pad("CREATED", colCreated),
	) + "\n"

	for _, r := range rows {
		// Status column: pad first so the colour sequences don't throw
		// off the column width. Lipgloss styled strings include ANSI
		// escapes, which len() ignores anyway, but padding the raw
		// string keeps things consistent.
		statusRaw := pad(string(r.status), colStatus)
		statusStyled := colourForStatus(r.status).Render(statusRaw)

		ip := r.ip
		if ip == "" {
			ip = "-"
		}

		row := pad(r.name, colName) +
			statusStyled +
			pad(ip, colIP) +
			pad(strconv.Itoa(r.cpus), colCPUs) +
			pad(strconv.Itoa(r.memory), colMem) +
			pad(humanRelative(now.Sub(r.created)), colCreated)

		out += listCellStyle.Render(row) + "\n"
	}
	return out
}

// colourForStatus picks the lipgloss style appropriate to the lifecycle
// state (spec §6).
func colourForStatus(s vm.Status) lipgloss.Style {
	switch s {
	case vm.StatusRunning:
		return statusRunningStyle
	case vm.StatusStopped:
		return statusStoppedStyle
	case vm.StatusCrashed:
		return statusCrashedStyle
	case vm.StatusCreating, vm.StatusStarting, vm.StatusStopping:
		return statusTransientStyle
	default:
		return statusOtherStyle
	}
}

// humanRelative renders a duration like "2h ago" / "3d ago" — same
// shape as the helper in cmd/image/list.go but kept local so the two
// commands aren't coupled by a private package.
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

// listSpaces returns a string of n space characters.
func listSpaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// printLine writes s + newline to w, swallowing errors. The same
// rationale as in cmd/image/pull.go: if writing to the cobra-managed
// stream fails there's nothing useful to do with the error.
func printLine(w io.Writer, s string) {
	_, _ = fmt.Fprintln(w, s)
}
