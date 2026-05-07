package vsock_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/testutil"
	"github.com/p5n-dev/forge/internal/vsock"
)

func TestListener_AcceptsReadyMessage(t *testing.T) {
	dir := testutil.SockTempDir(t)
	sockPath := filepath.Join(dir, "vsock.sock")

	lis, err := vsock.Listen(sockPath)
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()

	go func() {
		// Simulate the VM sending the ready message via vfkit's socket bridge.
		conn, err := net.Dial("unix", sockPath)
		require.NoError(t, err)
		_, _ = conn.Write([]byte("ready addr=192.168.127.42\n"))
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	msg, err := lis.WaitReady(ctx)
	require.NoError(t, err)
	assert.Equal(t, "192.168.127.42", msg.IP)
}

func TestListener_TimeoutBeforeMessage(t *testing.T) {
	dir := testutil.SockTempDir(t)
	sockPath := filepath.Join(dir, "vsock.sock")

	lis, err := vsock.Listen(sockPath)
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = lis.WaitReady(ctx)
	require.Error(t, err)
}

func TestListener_RejectsMalformedMessage(t *testing.T) {
	dir := testutil.SockTempDir(t)
	sockPath := filepath.Join(dir, "vsock.sock")

	lis, err := vsock.Listen(sockPath)
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()

	go func() {
		conn, err := net.Dial("unix", sockPath)
		require.NoError(t, err)
		_, _ = conn.Write([]byte("garbage data not in expected format\n"))
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = lis.WaitReady(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
}

func TestListener_SocketPath(t *testing.T) {
	dir := testutil.SockTempDir(t)
	sockPath := filepath.Join(dir, "vsock.sock")

	lis, err := vsock.Listen(sockPath)
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()

	assert.Equal(t, sockPath, lis.SocketPath())
}
