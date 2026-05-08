package env

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// WaitForSSH polls sockPath until vfkit's listener accepts a connection
// AND the in-VM sshd writes back a recognisable SSH-2.0 banner. Used by
// `forge env start` to know the VM is reachable end-to-end without
// relying on the boot-ready vsock signal (which only fires on first
// boot — cloud-init's runcmd and the base image's forge-ready.service
// are both first-boot-only).
//
// A successful banner read proves the full bridge:
//
//	host unix-socket → vfkit → vsock → in-VM forge-ssh-vsock.service →
//	sshd.
//
// Returns nil on success, ctx.Err() on cancellation, or a "not
// reachable after <timeout>" error on deadline expiry.
func WaitForSSH(ctx context.Context, sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		banner, err := probeSSHBanner(sockPath)
		if err == nil && strings.HasPrefix(banner, "SSH-2.0-") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh socket %s not reachable after %s (last error: %v)", sockPath, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// probeSSHBanner opens sockPath as a Unix socket and reads up to the
// first 64 bytes the peer sends. sshd's first line is the version
// banner, e.g. "SSH-2.0-OpenSSH_9.2p1 Debian-2\r\n".
func probeSSHBanner(sockPath string) (string, error) {
	if _, err := os.Stat(sockPath); err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", err
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	return string(buf[:n]), nil
}
