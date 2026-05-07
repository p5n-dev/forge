package progress_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/p5n-dev/forge/internal/progress"
)

func TestNop_DoesNothing(t *testing.T) {
	p := progress.Nop()
	done := p.Step("anything")
	done(nil)
	done(errors.New("ignored"))
	// Nothing to assert on output — the point is that it doesn't panic
	// and accepts arbitrary call sequences.
}

func TestPlain_SuccessAndFailure(t *testing.T) {
	var buf bytes.Buffer
	p := progress.NewPlain(&buf)

	done := p.Step("first thing")
	done(nil)

	done = p.Step("second thing")
	done(errors.New("kaboom"))

	got := buf.String()
	// Each step should produce a "→ ... ..." start line and a final
	// ✓ or ✗ line. They appear in order.
	expected := []string{
		"→ first thing...\n",
		"✓ first thing\n",
		"→ second thing...\n",
		"✗ second thing: kaboom\n",
	}
	idx := 0
	for _, want := range expected {
		j := strings.Index(got[idx:], want)
		if j < 0 {
			t.Fatalf("expected substring %q not found at or after offset %d in:\n%s", want, idx, got)
		}
		idx += j + len(want)
	}
}

// TestSpinner_ColoursStatusIndicators is a regression guard: a future
// refactor that drops the `successStyle.Render(...)` calls would leave
// the spinner monochrome with no test failure. The test forces lipgloss
// to emit ANSI codes for a buffer (which it would otherwise treat as a
// non-tty) and asserts each indicator carries the expected SGR code.
func TestSpinner_ColoursStatusIndicators(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	var buf bytes.Buffer
	p := progress.NewSpinner(&buf)

	done := p.Step("step A")
	// Let the spinner goroutine schedule + emit the first frame
	// before we close the step. 20ms is well over scheduler latency.
	time.Sleep(20 * time.Millisecond)
	done(nil)

	done = p.Step("step B")
	time.Sleep(20 * time.Millisecond)
	done(errors.New("boom"))

	out := buf.String()
	// 95 = bright magenta (spinner frame), 92 = bright green (✓),
	// 91 = bright red (✗). The exact SGR strings come from lipgloss
	// rendering ANSI16 profile under CLICOLOR_FORCE; if lipgloss
	// changes its rendering we want the test to fail visibly.
	assert.Contains(t, out, "\033[95m", "spinner frame should be magenta")
	assert.Contains(t, out, "92m", "success indicator should be green")
	assert.Contains(t, out, "91m", "failure indicator should be red")
}

func TestAuto_PlainForNonTTY(t *testing.T) {
	// A bytes.Buffer is not an *os.File so Auto must pick the plain
	// implementation. We assert that by checking the rendered output
	// matches the plain format.
	var buf bytes.Buffer
	p := progress.Auto(&buf)
	done := p.Step("xyz")
	done(nil)
	out := buf.String()
	assert.Contains(t, out, "→ xyz...")
	assert.Contains(t, out, "✓ xyz")
	// Spinner output would contain a CR (\r) — plain output never
	// does, so this distinguishes plain from spinner.
	assert.NotContains(t, out, "\r")
}
