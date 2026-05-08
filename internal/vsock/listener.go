// Package vsock listens on a Unix domain socket that vfkit bridges to the
// guest's virtio-vsock CID:port. The forge-ready service inside the VM
// connects to that vsock once cloud-init finishes and writes a single line
// of the form "ready addr=<ip>\n", which lets `forge env create` block
// until the VM is actually ready.
package vsock

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
)

// Message is a parsed boot-complete payload from the guest.
type Message struct {
	// IP is the address the guest reported via `hostname -I`.
	IP string
}

// Listener wraps a Unix-socket server that vfkit forwards vsock traffic to.
type Listener struct {
	path string
	ln   net.Listener
}

// Listen opens a Unix-domain listener at sockPath. vfkit should be invoked
// with `--device virtio-vsock,port=N,socketURL=<sockPath>,listen=true`
// (bare path, no unix:// scheme — see internal/vm/vfkit.go for why) so
// it forwards the guest's vsock connection to this socket.
func Listen(sockPath string) (*Listener, error) {
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", sockPath, err)
	}
	return &Listener{path: sockPath, ln: ln}, nil
}

// SocketPath returns the Unix-domain socket path this listener is bound to.
func (l *Listener) SocketPath() string {
	return l.path
}

// WaitReady blocks until the guest connects and sends a well-formed ready
// message, the context expires, or the listener is closed. The first
// well-formed message wins; subsequent connections are not awaited.
func (l *Listener) WaitReady(ctx context.Context) (*Message, error) {
	type result struct {
		msg *Message
		err error
	}
	results := make(chan result, 1)

	go func() {
		conn, err := l.ln.Accept()
		if err != nil {
			results <- result{err: fmt.Errorf("accepting vsock connection: %w", err)}
			return
		}
		defer func() { _ = conn.Close() }()

		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			results <- result{err: fmt.Errorf("reading ready line: %w", err)}
			return
		}

		msg, err := parseReadyLine(line)
		results <- result{msg: msg, err: err}
	}()

	select {
	case <-ctx.Done():
		// Closing the listener unblocks the Accept goroutine.
		_ = l.ln.Close()
		return nil, ctx.Err()
	case r := <-results:
		return r.msg, r.err
	}
}

// Close releases the underlying socket.
func (l *Listener) Close() error {
	return l.ln.Close()
}

// parseReadyLine extracts the IP from a "ready addr=<ip>" line.
func parseReadyLine(line string) (*Message, error) {
	line = strings.TrimSpace(line)
	const prefix = "ready addr="
	if !strings.HasPrefix(line, prefix) {
		return nil, fmt.Errorf("malformed ready message: %q", line)
	}
	ip := strings.TrimPrefix(line, prefix)
	if ip == "" {
		return nil, fmt.Errorf("malformed ready message: empty IP")
	}
	return &Message{IP: ip}, nil
}
