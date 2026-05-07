package env

import "path/filepath"

// SSHSocketPath returns the host-side Unix socket vfkit bridges to the
// guest's vsock port 22. `forge env connect` shells SSH through this
// socket via ProxyCommand, sidestepping the host IP routing table — and
// therefore any corporate VPN that may have hijacked the FORGE subnet.
func SSHSocketPath(envDir string) string {
	return filepath.Join(envDir, "ssh.sock")
}

// WorkspaceDir returns the host-side directory we clone the env's
// Forgejo repo into. The same directory is virtio-fs-shared into the
// VM at /home/forge/workspace, so the host and the in-VM agent see
// identical contents.
func WorkspaceDir(envDir string) string {
	return filepath.Join(envDir, "workspace")
}

// ForgejoSocketPath returns the host-side Unix socket vfkit bridges to
// the guest's Forgejo vsock channel. internal/forgejoproxy binds it
// and forwards each accepted connection to the actual Forgejo TCP
// endpoint on the host — the VPN-immune path that replaces dialing
// 192.168.127.1:<port> directly.
func ForgejoSocketPath(envDir string) string {
	return filepath.Join(envDir, "forgejo.sock")
}

// ForgejoProxyPIDPath returns the per-env PID file for the host-side
// forgejoproxy subprocess. Mirrors vfkit.pid in shape so env start /
// stop can reap it the same way.
func ForgejoProxyPIDPath(envDir string) string {
	return filepath.Join(envDir, "forgejo-proxy.pid")
}

// NetSocketPath returns the host-side unixgram (SOCK_DGRAM) socket
// vfkit's virtio-net device connects to. The internal/gvproxy
// subprocess (spawned by env create/start) binds this socket and
// runs the userspace netstack on it.
func NetSocketPath(envDir string) string {
	return filepath.Join(envDir, "net.sock")
}

// NetPIDPath returns the per-env PID file for the gvproxy subprocess.
func NetPIDPath(envDir string) string {
	return filepath.Join(envDir, "gvproxy.pid")
}
