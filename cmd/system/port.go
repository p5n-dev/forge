package system

import (
	"fmt"
	"net"
)

// findAvailablePort returns the first TCP port that nothing on the host
// is currently listening on, starting at preferred and probing upward
// for at most maxAttempts ports. Returns the preferred port immediately
// if it's free.
//
// There is an unavoidable TOCTOU window between "we successfully bound
// briefly" and "docker tries to bind for real". For the local-dev
// workflow this is fine — if docker still fails, the user gets the
// same friendly port-in-use error that already exists.
func findAvailablePort(preferred, maxAttempts int) (int, error) {
	for i := 0; i < maxAttempts; i++ {
		p := preferred + i
		if isPortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free TCP port in range %d-%d", preferred, preferred+maxAttempts-1)
}

func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
