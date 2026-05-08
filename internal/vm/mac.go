package vm

import (
	"crypto/rand"
	"fmt"
)

// GenerateMAC returns a random, locally-administered, unicast MAC
// address in the QEMU/libvirt-style 52:54:00 prefix.
//
// The 52:54:00 prefix is a long-standing convention for software-emulated
// NICs: the first octet has the locally-administered bit (0x02) set and
// the unicast bit (0x01) clear, which keeps Apple's Virtualization
// framework and most DHCP servers happy.
//
// Only the last three octets are randomised. If the system entropy
// source is unavailable (which would also break TLS, SSH, etc.) the
// function panics — callers are not expected to handle this.
func GenerateMAC() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The system has no entropy source; treat as fatal.
		panic(fmt.Sprintf("vm: reading random bytes for MAC: %v", err))
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}
