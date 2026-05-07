// Package testutil contains small helpers shared across FORGE's
// internal test suites. Production code does NOT depend on this
// package — it's covered by Go's `_test.go` build constraints
// only via the consumers, but kept in a normal package so it's
// reachable from every internal/* test without a _test.go cyclic.
package testutil

import (
	"os"
	"testing"
)

// SockTempDir returns a tempdir suitable for AF_UNIX sockets on
// every supported host.
//
// macOS limits sockaddr_un.sun_path to 104 bytes; combined with
// `t.TempDir()`'s long `/var/folders/<encoded>/T/<TestName>NNNN/001/`
// prefix and a socket filename, the limit is easy to exceed and bind
// returns EINVAL with a misleading "invalid argument" message. Linux
// allows 108 bytes and `/tmp` is short, so the issue only ever shows
// up on Macs running the unit suite.
//
// This wraps os.MkdirTemp under /tmp (a short, predictable parent on
// both OSes — `/tmp` is `/private/tmp` symlinked on macOS, ~9 chars
// after resolution) and registers a cleanup so the dir disappears at
// the end of the test. Tests that bind sockets should use this in
// place of t.TempDir().
func SockTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "forge-sock-*")
	if err != nil {
		t.Fatalf("creating short-path tempdir for AF_UNIX socket: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}
