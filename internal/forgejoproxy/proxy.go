// Package forgejoproxy implements a Unix-domain → TCP forwarder that
// FORGE spawns alongside vfkit so VMs can reach the host's Forgejo over
// vsock instead of the IP routing table.
//
// Why this exists: corporate VPN clients (Cisco AnyConnect, GlobalProtect,
// Zscaler …) intercept macOS traffic at the Network Extension layer,
// below the routing layer. Even with FORGE on gvproxy userspace
// networking, dialing the host-Forgejo by IP from inside the VM is
// brittle — IP-based paths are exactly what those Network Extensions
// can disrupt. The vsock channel sidesteps the IP stack entirely:
// vfkit's `--device virtio-vsock,port=N,socketURL=…,listen` dials a
// host-side Unix socket when the guest connects, and this package is
// what's listening on the other end of that socket.
//
// Wire shape, per env:
//
//	in-VM socat (TCP-LISTEN:4000 → VSOCK-CONNECT:2:4000)
//	      ↓ vsock
//	vfkit virtio-vsock device (port=4000, socketURL=<envDir>/forgejo.sock, listen)
//	      ↓ unix socket
//	this package's Proxy (UNIX-LISTEN → TCP:127.0.0.1:4000)
//	      ↓ tcp
//	the user's actual Forgejo (Docker container on the host)
package forgejoproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// Proxy is the unix-socket → TCP forwarder. One instance per env.
type Proxy struct {
	// SocketPath is the Unix-domain socket path Proxy binds. vfkit
	// must be invoked with socketURL pointing at the same path.
	SocketPath string
	// Target is the dial target for outbound connections — typically
	// "127.0.0.1:<forgejo-port>" on the macOS host.
	Target string
}

// Run binds SocketPath and forwards each accepted connection to Target.
// It blocks until ctx is cancelled or the listener fails.
//
// On shutdown Run closes the listener AND every in-flight bridged
// connection so the goroutines unblock from io.Copy and drain. Without
// the explicit conn-close, an HTTP keep-alive client could hold the
// pipe open indefinitely past ctx cancel.
func (p Proxy) Run(ctx context.Context) error {
	if p.SocketPath == "" {
		return errors.New("forgejoproxy: socket path is required")
	}
	if p.Target == "" {
		return errors.New("forgejoproxy: target is required")
	}

	// Best-effort remove a leftover socket file from a crashed previous
	// run — net.Listen would otherwise fail with EADDRINUSE. Same trap
	// as ssh.sock and the boot-ready vsock socket.
	_ = os.Remove(p.SocketPath)

	ln, err := net.Listen("unix", p.SocketPath)
	if err != nil {
		return fmt.Errorf("forgejoproxy: listening on %s: %w", p.SocketPath, err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = make(map[net.Conn]struct{})
	)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			// ctx cancellation is the intended shutdown path; the
			// resulting "use of closed network connection" from
			// Accept isn't an error worth surfacing.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("forgejoproxy: accept: %w", err)
		}
		mu.Lock()
		conns[conn] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
			}()
			p.bridge(conn)
		}()
	}
}

// bridge dials Target and splices bytes both ways until either side
// closes. Errors are swallowed — a closed connection is the expected
// terminal state, not an exception worth surfacing.
func (p Proxy) bridge(client net.Conn) {
	defer func() { _ = client.Close() }()

	upstream, err := net.Dial("tcp", p.Target)
	if err != nil {
		// Surface a hint to the connecting client (git, curl, …) so
		// the failure isn't completely silent. HTTP clients will
		// surface this as a malformed response, which is at least
		// less confusing than a blank disconnect.
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n"+
			"Content-Length: 0\r\n"+
			"X-Forge-Proxy-Error: dial "+p.Target+" failed\r\n\r\n")
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		// Half-close so the upstream sees EOF on its read side and
		// finishes its own response cleanly.
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(*net.UnixConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}
