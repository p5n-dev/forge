// Package gvproxy wraps github.com/containers/gvisor-tap-vsock to give
// FORGE VMs full outbound network connectivity even when the host is
// on a tunnel-all corporate VPN.
//
// Why this exists: vfkit's default `--device virtio-net,nat` mode uses
// Apple's vmnet kernel framework which performs NAT in-kernel. On a
// tunnel-all VPN, the resulting NAT'd packets are policy-distinguishable
// from Mac-process traffic and get dropped by the corp VPN's
// NEPacketTunnelProvider. The Mac browser keeps working; the VM does
// not. (See CLAUDE.md § "Networking model" + the project_corp_vpn
// memory.)
//
// gvisor-tap-vsock replaces vmnet entirely. vfkit is launched with
// `--device virtio-net,unixSocketPath=<envDir>/net.sock` (a SOCK_DGRAM
// unix socket); this package binds the other end and runs a userspace
// gVisor TCP/IP stack. Every outbound TCP/UDP/ICMP from the VM becomes
// a host-side `socket()` syscall — indistinguishable to the VPN policy
// engine from any other Mac process. Same approach Podman Machine and
// WSL2 use for their VM↔host networking.
//
// Wire shape:
//
//	VM virtio-net (enp0s1) ─┐
//	                        │  SOCK_DGRAM packets
//	vfkit unixSocketPath ───┤
//	                        ▼
//	      <envDir>/net.sock (this package binds it)
//	                        │
//	                        ▼
//	      gvisor netstack (DHCP+DNS+NAT, all userspace)
//	                        │
//	                        ▼
//	      host net.Dial / net.Listen calls (regular Mac processes)
package gvproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// gvisor-tap-vsock's standard demo subnet, also used by Podman Machine.
// Picked to be unlikely to collide with corporate VPN subnets, and
// distinct from the legacy 192.168.64.0/24 vmnet range FORGE used
// before this package existed — keeping the two distinct makes the
// architectural change obvious in routing tables.
const (
	DefaultSubnet     = "192.168.127.0/24"
	DefaultGatewayIP  = "192.168.127.1"
	DefaultGatewayMAC = "5a:94:ef:e4:0c:dd"
	// HostIP is the VM-side address gvproxy translates to 127.0.0.1
	// on the macOS host. Lets the VM dial host-local services
	// (Forgejo, etc.) via a known stable IP without us having to
	// detect the host's primary interface address.
	DefaultHostIP = "192.168.127.254"
	DefaultMTU    = 1500
)

// Config configures a single gvproxy instance. One per FORGE env.
type Config struct {
	// SocketPath is the unixgram socket path vfkit will be told to
	// connect to via `--device virtio-net,unixSocketPath=<path>`.
	// Required. Removed and re-bound on Run().
	SocketPath string

	// Subnet, GatewayIP, GatewayMAC, HostIP, MTU default to the
	// Default* constants above. Override only if you have a reason —
	// the defaults are what FORGE's cloud-init network-config and
	// IP allocator are tuned to.
	Subnet     string
	GatewayIP  string
	GatewayMAC string
	HostIP     string
	MTU        int

	// CaptureFile, when set, records every L2 packet to a pcap file
	// readable by Wireshark. Useful for debugging "the VM can't ping
	// 1.1.1.1 anymore" issues. Empty = no capture.
	CaptureFile string
}

// Run binds SocketPath, accepts the vfkit unixgram conn, and runs the
// gVisor netstack until ctx is cancelled. It is the host-process
// counterpart to vfkit's virtio-net,unixSocketPath device.
//
// vfkit will refuse to dial a socket that doesn't exist yet, so Run
// MUST be invoked (and SocketPath bound) before vfkit is started. The
// `forge env start` orchestration spawns gvproxy first specifically
// for this reason.
func (c Config) Run(ctx context.Context) error {
	if c.SocketPath == "" {
		return errors.New("gvproxy: socket path is required")
	}

	cfg := c.buildConfiguration()
	vn, err := virtualnetwork.New(cfg)
	if err != nil {
		return fmt.Errorf("gvproxy: creating virtual network: %w", err)
	}

	// Best-effort remove a leftover socket file from a crashed previous
	// run — ListenUnixgram would otherwise fail with EADDRINUSE. Same
	// trap as ssh.sock and the boot-ready vsock socket.
	_ = os.Remove(c.SocketPath)

	// gvisor-tap-vsock's ListenUnixgram parses its argument as a URL
	// (it shares the codepath with the vsock transport). Bare paths
	// fail with "unexpected scheme"; the canonical form is
	// `unixgram://<path>`. We accept either at our boundary so the
	// caller doesn't have to know.
	endpoint := c.SocketPath
	if !strings.HasPrefix(endpoint, "unixgram://") {
		endpoint = "unixgram://" + endpoint
	}
	conn, err := transport.ListenUnixgram(endpoint)
	if err != nil {
		return fmt.Errorf("gvproxy: listen unixgram %s: %w", c.SocketPath, err)
	}
	defer func() {
		_ = conn.Close()
		_ = os.Remove(c.SocketPath)
	}()

	// Closing the listener on ctx cancellation is what unblocks the
	// AcceptVfkit and AcceptVfkit-loop goroutines below. We don't poll
	// ctx ourselves inside the gvisor stack — it has its own teardown.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	vfkitConn, err := transport.AcceptVfkit(conn)
	if err != nil {
		// ctx-cancel induced shutdown is expected; not an error.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("gvproxy: accept vfkit: %w", err)
	}

	if err := vn.AcceptVfkit(ctx, vfkitConn); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("gvproxy: vfkit accept loop: %w", err)
	}
	return nil
}

// buildConfiguration applies defaults and converts our Config into the
// gvisor-tap-vsock library's Configuration struct. Exposed only so
// tests can inspect the resulting values.
func (c Config) buildConfiguration() *types.Configuration {
	if c.Subnet == "" {
		c.Subnet = DefaultSubnet
	}
	if c.GatewayIP == "" {
		c.GatewayIP = DefaultGatewayIP
	}
	if c.GatewayMAC == "" {
		c.GatewayMAC = DefaultGatewayMAC
	}
	if c.HostIP == "" {
		c.HostIP = DefaultHostIP
	}
	if c.MTU == 0 {
		c.MTU = DefaultMTU
	}

	return &types.Configuration{
		MTU:               c.MTU,
		Subnet:            c.Subnet,
		GatewayIP:         c.GatewayIP,
		GatewayMacAddress: c.GatewayMAC,
		// Reach the host's loopback at HostIP from inside the VM.
		// gvisor handles the rewrite — the VM just sees a normal IP
		// it can dial like any other host.
		NAT: map[string]string{
			c.HostIP: "127.0.0.1",
		},
		// Make HostIP answer ARP so the VM's first packet to it
		// resolves correctly without us pre-populating an ARP entry.
		GatewayVirtualIPs: []string{c.HostIP},
		// Built-in DNS records so VM-side scripts can use stable
		// names (gateway.forge.internal, host.forge.internal)
		// instead of hardcoding IPs that may change later.
		DNS: []types.Zone{
			{
				Name: "forge.internal.",
				Records: []types.Record{
					{Name: "gateway", IP: net.ParseIP(c.GatewayIP)},
					{Name: "host", IP: net.ParseIP(c.HostIP)},
				},
			},
		},
		CaptureFile: c.CaptureFile,
	}
}
