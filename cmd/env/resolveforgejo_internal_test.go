package env

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/p5n-dev/forge/internal/config"
)

func TestResolveForgejo_ConfiguredURL(t *testing.T) {
	// When the user has set forgejo.url in ~/.forge/config.yaml, the
	// configured URL is returned verbatim — both host API and VM-side
	// agents reach it the same way (vsock-bridged).
	cfg := config.GlobalConfig{}
	cfg.Forgejo.URL = "http://localhost:4000"

	base, target, port := resolveForgejo(cfg)
	assert.Equal(t, "http://localhost:4000", base)
	assert.Equal(t, "127.0.0.1:4000", target,
		"host-side proxy must dial loopback — Docker publishes Forgejo on 127.0.0.1:<port>")
	assert.Equal(t, 4000, port)
}

func TestResolveForgejo_NoConfigFallsBackToDefaultPort(t *testing.T) {
	base, target, port := resolveForgejo(config.GlobalConfig{})
	assert.Contains(t, base, "http://localhost:")
	assert.Contains(t, target, "127.0.0.1:")
	assert.Greater(t, port, 0)
}

func TestPortFromURL(t *testing.T) {
	cases := map[string]int{
		"http://localhost:4000":           4000,
		"https://forgejo.corp.internal":   443,
		"http://forgejo.corp.internal":    80,
		"http://forgejo.corp.internal:80": 80,
	}
	for in, want := range cases {
		assert.Equal(t, want, portFromURL(in), "input=%s", in)
	}
}
