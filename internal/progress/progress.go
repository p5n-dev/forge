// Package progress reports the status of long-running CLI operations
// to the user. The Progress interface lets orchestration code stay
// agnostic about presentation: production callers wire up a TTY
// spinner; redirected output gets a plain per-line log; tests use Nop.
package progress

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Status-indicator styles. Colours match the rest of the FORGE CLI
// (see cmd/image/pull.go for the same green/red used elsewhere). They
// are applied only on the spinner path — i.e. when stdout is a TTY —
// so non-interactive output stays free of ANSI escape codes.
var (
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	spinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
)

// Progress is a thin reporter for long-running steps.
//
// Step(desc) starts a step and returns a done function. The caller
// must invoke done exactly once: pass nil on success, or the error
// on failure. The implementation handles all rendering — callers
// should not write to the same out stream while a step is in flight.
type Progress interface {
	Step(description string) func(err error)
}

// Auto returns the appropriate Progress for out: a spinner when out
// is a TTY, a plain per-line reporter otherwise.
func Auto(out io.Writer) Progress {
	if isTTY(out) {
		return NewSpinner(out)
	}
	return NewPlain(out)
}

// Nop returns a Progress that discards all calls. Suitable for tests
// and for code paths where progress reporting isn't wanted.
func Nop() Progress { return nopProgress{} }

type nopProgress struct{}

func (nopProgress) Step(string) func(error) { return func(error) {} }

// NewPlain returns a Progress that prints one line per step state
// transition: "→ <desc>..." on start, "✓ <desc>" or "✗ <desc>: <err>"
// on completion. Suitable for non-interactive output (CI, log files).
func NewPlain(out io.Writer) Progress { return &plainProgress{out: out} }

type plainProgress struct {
	out io.Writer
	mu  sync.Mutex
}

func (p *plainProgress) Step(desc string) func(error) {
	p.mu.Lock()
	_, _ = fmt.Fprintf(p.out, "→ %s...\n", desc)
	p.mu.Unlock()
	return func(err error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil {
			_, _ = fmt.Fprintf(p.out, "✗ %s: %v\n", desc, err)
			return
		}
		_, _ = fmt.Fprintf(p.out, "✓ %s\n", desc)
	}
}

// NewSpinner returns a Progress that animates a braille spinner on
// a single line for the duration of each step, then replaces it with
// a final ✓ / ✗ marker on the same row. Frames update every 100ms.
func NewSpinner(out io.Writer) Progress { return &spinnerProgress{out: out} }

type spinnerProgress struct {
	out io.Writer
	mu  sync.Mutex // serialises overlapping step lifecycles (defensive)
}

var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

func (p *spinnerProgress) Step(desc string) func(error) {
	p.mu.Lock() // released by the returned done()
	stop := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		// Print the first frame immediately so users see something
		// without waiting for the first tick.
		_, _ = fmt.Fprintf(p.out, "\r%s %s", spinnerStyle.Render(spinnerFrames[0]), desc)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				i = (i + 1) % len(spinnerFrames)
				_, _ = fmt.Fprintf(p.out, "\r%s %s", spinnerStyle.Render(spinnerFrames[i]), desc)
			}
		}
	}()

	return func(err error) {
		close(stop)
		<-finished
		// \033[K clears from the cursor to end-of-line, so a long
		// in-progress description doesn't bleed past a shorter final.
		if err != nil {
			_, _ = fmt.Fprintf(p.out, "\r\033[K%s %s: %v\n", errorStyle.Render("✗"), desc, err)
		} else {
			_, _ = fmt.Fprintf(p.out, "\r\033[K%s %s\n", successStyle.Render("✓"), desc)
		}
		p.mu.Unlock()
	}
}

// isTTY reports whether out is the controlling terminal — i.e. whether
// in-place ANSI animation is appropriate.
func isTTY(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
