package system

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindAvailablePort_PreferredIsFree(t *testing.T) {
	// Pick a high port likely free; if not, bump preferred up.
	preferred := 39000
	for !isPortFree(preferred) {
		preferred++
	}
	got, err := findAvailablePort(preferred, 5)
	require.NoError(t, err)
	assert.Equal(t, preferred, got)
}

func TestFindAvailablePort_FallsThroughTakenOnes(t *testing.T) {
	// Bind a port to make it "taken" and verify we skip it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	taken := ln.Addr().(*net.TCPAddr).Port
	got, err := findAvailablePort(taken, 5)
	require.NoError(t, err)
	assert.NotEqual(t, taken, got, "must skip the bound port")
	assert.True(t, got > taken, "must move forward from the taken one")
}

func TestFindAvailablePort_GivesUp(t *testing.T) {
	// Bind several consecutive ports starting at a chosen base, then ask
	// findAvailablePort to scan only those — it should fail.
	const span = 3
	base := 40000
	listeners := make([]net.Listener, 0, span)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for i := 0; i < span; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
		if err != nil {
			t.Skip("could not reserve port range for test:", err)
		}
		listeners = append(listeners, ln)
	}

	_, err := findAvailablePort(base, span)
	require.Error(t, err)
}
