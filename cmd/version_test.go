package cmd

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrintVersion_CoreFields(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	version = "v9.9.9-test"

	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()

	assert.True(t, strings.HasPrefix(out, "forge:\n"), "should start with header: %q", out)
	assert.Contains(t, out, " Version:    v9.9.9-test\n")
	assert.Contains(t, out, " Go version: "+runtime.Version()+"\n")
	assert.Contains(t, out, " OS/Arch:    "+runtime.GOOS+"/"+runtime.GOARCH+"\n")
}

func TestPrintVersion_LinesAreWellFormed(t *testing.T) {
	// Whatever VCS state the test binary was built with, every non-header
	// line should be " Key:<spaces>Value" so the block stays aligned.
	var buf bytes.Buffer
	printVersion(&buf)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assert.Equal(t, "forge:", lines[0])
	for _, line := range lines[1:] {
		assert.True(t, strings.HasPrefix(line, " "), "field line should be indented: %q", line)
		assert.Contains(t, line, ":", "field line should be key:value: %q", line)
	}
}
