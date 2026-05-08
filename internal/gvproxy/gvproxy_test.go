package gvproxy

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildConfiguration_Defaults(t *testing.T) {
	cfg := Config{SocketPath: "/tmp/x"}.buildConfiguration()

	assert.Equal(t, DefaultSubnet, cfg.Subnet)
	assert.Equal(t, DefaultGatewayIP, cfg.GatewayIP)
	assert.Equal(t, DefaultGatewayMAC, cfg.GatewayMacAddress)
	assert.Equal(t, DefaultMTU, cfg.MTU)

	// HostIP must be NAT-translated to 127.0.0.1 so the VM can reach
	// host-local services (Forgejo, etc.) via a stable address.
	assert.Equal(t, "127.0.0.1", cfg.NAT[DefaultHostIP],
		"VM-side HostIP must rewrite to host loopback")

	// HostIP also has to answer ARP — without a virtual IP entry,
	// the VM's first packet to it would never resolve and quietly drop.
	assert.Contains(t, cfg.GatewayVirtualIPs, DefaultHostIP)

	// Built-in DNS entries so scripts can hardcode names, not IPs.
	require.Len(t, cfg.DNS, 1)
	zone := cfg.DNS[0]
	assert.Equal(t, "forge.internal.", zone.Name)
	gotNames := map[string]net.IP{}
	for _, r := range zone.Records {
		gotNames[r.Name] = r.IP
	}
	assert.Equal(t, net.ParseIP(DefaultGatewayIP), gotNames["gateway"])
	assert.Equal(t, net.ParseIP(DefaultHostIP), gotNames["host"])
}

func TestBuildConfiguration_Overrides(t *testing.T) {
	c := Config{
		SocketPath: "/tmp/x",
		Subnet:     "10.7.0.0/24",
		GatewayIP:  "10.7.0.1",
		GatewayMAC: "aa:bb:cc:dd:ee:ff",
		HostIP:     "10.7.0.254",
		MTU:        1280,
	}
	cfg := c.buildConfiguration()
	assert.Equal(t, "10.7.0.0/24", cfg.Subnet)
	assert.Equal(t, "10.7.0.1", cfg.GatewayIP)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", cfg.GatewayMacAddress)
	assert.Equal(t, 1280, cfg.MTU)
	assert.Equal(t, "127.0.0.1", cfg.NAT["10.7.0.254"])
}

func TestRun_RejectsEmptySocketPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Config{}.Run(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "socket path")
}
