package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/vm"
)

func TestDoctorReport_AnyFailed(t *testing.T) {
	r := doctorReport{checks: []doctorCheck{
		{name: "a", status: statusPass},
		{name: "b", status: statusSkip},
	}}
	assert.False(t, r.anyFailed())
	r.add(doctorCheck{name: "c", status: statusFail})
	assert.True(t, r.anyFailed())
}

func TestRenderDoctor_GlyphsAndHints(t *testing.T) {
	r := doctorReport{checks: []doctorCheck{
		{name: "vfkit", status: statusPass, msg: "v0.6.3"},
		{name: "Host route", status: statusFail, msg: "via utun4", hint: "VPN is hijacking the subnet."},
		{name: "LaunchAgent", status: statusSkip, msg: "off-darwin"},
	}}

	var buf bytes.Buffer
	renderDoctor(&buf, r)
	out := buf.String()

	assert.Contains(t, out, "vfkit")
	assert.Contains(t, out, "v0.6.3")
	assert.Contains(t, out, "Host route")
	assert.Contains(t, out, "via utun4")
	// FAIL hint must surface so the user knows what action to take.
	assert.Contains(t, out, "VPN is hijacking the subnet.")
	assert.Contains(t, out, "LaunchAgent")
	// PASS lines should not print a hint even if one were set.
	withHintOnPass := doctorReport{checks: []doctorCheck{
		{name: "x", status: statusPass, msg: "ok", hint: "should not appear"},
	}}
	var buf2 bytes.Buffer
	renderDoctor(&buf2, withHintOnPass)
	assert.NotContains(t, buf2.String(), "should not appear")
}

// writeRunningEnv lays down a state.json so checkEnvs can iterate it.
func writeRunningEnv(t *testing.T, base, name, ip string, status vm.Status) string {
	t.Helper()
	dir := filepath.Join(base, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	state := &vm.State{
		Name:      name,
		Status:    status,
		IP:        ip,
		MAC:       "52:54:00:00:00:01",
		CreatedAt: time.Now(),
	}
	require.NoError(t, state.Save(dir))
	return dir
}

func TestCheckEnvs_StoppedEnvIsSkipped(t *testing.T) {
	base := t.TempDir()
	writeRunningEnv(t, base, "demo", "192.168.127.42", vm.StatusStopped)

	checks := checkEnvs(base)
	require.Len(t, checks, 1)
	assert.Equal(t, statusSkip, checks[0].status)
	assert.Contains(t, checks[0].msg, "stopped")
}

func TestCheckEnvs_RunningWithDeadVfkitFails(t *testing.T) {
	// State says running but no vfkit.pid file exists — IsAlive
	// returns (false, nil), and checkSingleEnv should report FAIL with
	// the recovery hint.
	base := t.TempDir()
	writeRunningEnv(t, base, "demo", "192.168.127.42", vm.StatusRunning)

	checks := checkEnvs(base)
	require.Len(t, checks, 1)
	assert.Equal(t, statusFail, checks[0].status)
	assert.Contains(t, checks[0].msg, "vfkit pid is gone")
	assert.Contains(t, checks[0].hint, "forge env start demo")
}

func TestCheckEnvs_IgnoresLooseFilesAndUnmanagedDirs(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "README.md"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "scratch"), 0o755))

	checks := checkEnvs(base)
	assert.Empty(t, checks, "loose files and dirs without state.json should not produce checks")
}

func TestCheckEnvs_MissingBaseDirReturnsNil(t *testing.T) {
	// Fresh install: ~/.forge/envs doesn't exist. Should not error.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	assert.Nil(t, checkEnvs(missing))
}
