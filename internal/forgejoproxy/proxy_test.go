package forgejoproxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/forgejoproxy"
	"github.com/p5n-dev/forge/internal/testutil"
)

func TestProxy_ForwardsUnixToTCP_HTTP(t *testing.T) {
	// Stand up a tiny HTTP origin and have the proxy front it via a
	// unix socket. End-to-end check: a unix-socket dial reaches the
	// origin and the response body comes back intact.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from origin: "+r.URL.Path)
	}))
	defer origin.Close()

	sockPath := filepath.Join(testutil.SockTempDir(t), "forgejo.sock")
	target := strings.TrimPrefix(origin.URL, "http://")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- forgejoproxy.Proxy{SocketPath: sockPath, Target: target}.Run(ctx)
	}()

	// Wait until the proxy has bound the socket.
	require.Eventually(t, func() bool {
		_, err := net.Dial("unix", sockPath)
		return err == nil
	}, time.Second, 10*time.Millisecond, "proxy never bound %s", sockPath)

	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	resp, err := client.Get("http://forgejo/path/check")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello from origin: /path/check", string(body))

	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err, "Run should exit cleanly on ctx cancel")
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestProxy_DialFailureReturns502(t *testing.T) {
	// Pick a definitely-closed TCP target. The proxy should not crash
	// when the upstream is unreachable — it should write a 502 to the
	// client so the failure is visible in HTTP-aware clients.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := closed.Addr().String()
	require.NoError(t, closed.Close())

	sockPath := filepath.Join(testutil.SockTempDir(t), "forgejo.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = forgejoproxy.Proxy{SocketPath: sockPath, Target: addr}.Run(ctx)
	}()
	require.Eventually(t, func() bool {
		_, err := net.Dial("unix", sockPath)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	conn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	assert.Contains(t, string(buf[:n]), "502 Bad Gateway")
}

func TestProxy_RejectsEmptyConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.Error(t, forgejoproxy.Proxy{}.Run(ctx))
	assert.Error(t, forgejoproxy.Proxy{SocketPath: "/tmp/x"}.Run(ctx))
}
