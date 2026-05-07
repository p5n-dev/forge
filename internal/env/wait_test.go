package env

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/testutil"
)

// TestWaitForSSH_Succeeds spins up a tiny Unix-domain "sshd" on a
// temp socket that emits the canonical SSH-2.0 banner on accept, and
// confirms WaitForSSH returns cleanly.
func TestWaitForSSH_Succeeds(t *testing.T) {
	sock := filepath.Join(testutil.SockTempDir(t), "ssh.sock")
	stop := startBannerServer(t, sock, "SSH-2.0-OpenSSH_9.2p1 Debian-2\r\n")
	t.Cleanup(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, WaitForSSH(ctx, sock, 2*time.Second))
}

// TestWaitForSSH_TimesOutWhenSocketMissing confirms the deadline
// branch fires when the socket file never appears.
func TestWaitForSSH_TimesOutWhenSocketMissing(t *testing.T) {
	missing := filepath.Join(testutil.SockTempDir(t), "never.sock")
	err := WaitForSSH(context.Background(), missing, 200*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable")
}

// TestWaitForSSH_RejectsNonSSHBanner makes sure a peer that accepts
// connections but doesn't speak SSH (a stray socat from a previous
// run, say) still trips the deadline rather than returning success.
func TestWaitForSSH_RejectsNonSSHBanner(t *testing.T) {
	sock := filepath.Join(testutil.SockTempDir(t), "ssh.sock")
	stop := startBannerServer(t, sock, "HELLO\r\n")
	t.Cleanup(stop)

	err := WaitForSSH(context.Background(), sock, 300*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable")
}

// TestWaitForSSH_HonoursContextCancel returns ctx.Err() promptly when
// the caller cancels.
func TestWaitForSSH_HonoursContextCancel(t *testing.T) {
	missing := filepath.Join(testutil.SockTempDir(t), "never.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitForSSH(ctx, missing, 5*time.Second)
	assert.ErrorIs(t, err, context.Canceled)
}

// startBannerServer listens on sockPath, emits banner on every accept,
// and shuts down on the returned stop func. Used by the WaitForSSH
// tests to stand in for sshd-via-vsock.
func startBannerServer(t *testing.T, sockPath, banner string) func() {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			_, _ = conn.Write([]byte(banner))
			_ = conn.Close()
		}
	}()
	return func() {
		_ = ln.Close()
		<-done
	}
}
