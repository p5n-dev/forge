package vm_test

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/vm"
)

func TestGenerateMAC_Format(t *testing.T) {
	mac := vm.GenerateMAC()

	parsed, err := net.ParseMAC(mac)
	require.NoError(t, err, "GenerateMAC must produce a parseable MAC")
	require.Len(t, parsed, 6)

	parts := strings.Split(mac, ":")
	require.Len(t, parts, 6)
	for _, p := range parts {
		assert.Len(t, p, 2)
		// All-lowercase hex per the QEMU/libvirt convention used by Apple VF.
		assert.Equal(t, strings.ToLower(p), p, "MAC octets should be lowercase")
	}
}

func TestGenerateMAC_OUIPrefix(t *testing.T) {
	mac := vm.GenerateMAC()
	assert.True(t, strings.HasPrefix(strings.ToLower(mac), "52:54:00:"),
		"GenerateMAC should use the locally-administered 52:54:00 prefix; got %q", mac)
}

func TestGenerateMAC_LocallyAdministeredAndUnicast(t *testing.T) {
	mac := vm.GenerateMAC()
	parsed, err := net.ParseMAC(mac)
	require.NoError(t, err)

	// Locally-administered: bit 1 of the first octet must be set.
	assert.Equal(t, byte(0x02), parsed[0]&0x02, "locally-administered bit must be set")
	// Unicast: bit 0 of the first octet must be clear.
	assert.Equal(t, byte(0x00), parsed[0]&0x01, "unicast bit must be clear")
}

func TestGenerateMAC_Randomness(t *testing.T) {
	// Generating a few should produce different values almost certainly.
	seen := make(map[string]struct{}, 16)
	for i := 0; i < 16; i++ {
		seen[vm.GenerateMAC()] = struct{}{}
	}
	assert.Greater(t, len(seen), 1, "GenerateMAC should produce varied results")
}
