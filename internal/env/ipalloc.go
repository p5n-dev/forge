package env

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/p5n-dev/forge/internal/vm"
)

// Static-IP plumbing for gvproxy's userspace netstack (see
// internal/gvproxy). Each FORGE env gets its own gvproxy instance and
// therefore its own private /24 — so per-env IP uniqueness isn't
// strictly required for correctness, but keeping each env on a
// distinguishable IP makes pcap analysis and routing-table reads
// less confusing.
const (
	NetworkSubnet  = "192.168.127.0/24" // matches gvproxy.DefaultSubnet
	NetworkPrefix  = 24
	NetworkGateway = "192.168.127.1" // matches gvproxy.DefaultGatewayIP
	// gvproxy serves DNS at the gateway IP (forwards to the host's
	// resolvers). 1.1.1.1 stays as a fallback so the VM still resolves
	// names if the gvproxy DNS service is wedged for some reason.
	NetworkDNSPrimary  = "192.168.127.1"
	NetworkDNSFallback = "1.1.1.1"

	// allocStart skips the gateway (.1), .254 (gvproxy's host-NAT
	// virtual IP — see gvproxy.DefaultHostIP), and the network /
	// broadcast bytes.
	allocStart = 42
	allocEnd   = 253
)

// AllocateIP picks the lowest unused IP in the static range across all
// envs under envBaseDir. It scans every `<env>/state.json` and avoids
// any address already recorded. Returns dotted-decimal "192.168.127.X".
//
// The caller persists the returned IP into the new env's state.json
// before the next allocation can see it. This is single-writer by
// design — `forge env create` runs serially per shell. Two parallel
// `forge env create` invocations could pick the same IP; that's a
// known limitation, documented in the spec.
//
// Allocation is bottom-up so destroyed-env IPs are reused before the
// pool grows.
func AllocateIP(envBaseDir string) (string, error) {
	used, err := collectUsedIPs(envBaseDir)
	if err != nil {
		return "", err
	}
	for last := allocStart; last <= allocEnd; last++ {
		ip := fmt.Sprintf("192.168.127.%d", last)
		if !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no free IPs in %s — destroy some envs first", NetworkSubnet)
}

func collectUsedIPs(envBaseDir string) (map[string]bool, error) {
	used := map[string]bool{}
	entries, err := os.ReadDir(envBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return used, nil
		}
		return nil, fmt.Errorf("listing envs in %s: %w", envBaseDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		state, err := vm.LoadState(filepath.Join(envBaseDir, e.Name()))
		if err != nil {
			// Missing or malformed state.json — not a managed env, skip.
			continue
		}
		if state.IP != "" {
			used[state.IP] = true
		}
	}
	return used, nil
}
