package forgejo

// Pinned Forgejo container image — refreshed by ./scripts/refresh-pins.sh.
//
// Soak rule: only pin versions that have been published for at least
// 14 days, so the wider community has had time to surface bugs and
// CVEs before FORGE adopts them. The tag is included for human
// readability; what docker actually verifies is the @sha256 digest.
//
// The string literals on the lines below are rewritten in place by
// the refresh script using simple `sed` patterns — keep them on a
// single line and don't move the constant names.
const (
	defaultImageRepo   = "codeberg.org/forgejo/forgejo"
	defaultImageTag    = "15.0.0"                                                                  // pin:forgejo-tag
	defaultImageDigest = "sha256:e87297f8ec332240228d24f2d6f0b408e30749f4ed166c1e5cf30a2288723794" // pin:forgejo-digest
)

// defaultImage builds the digest-locked image reference docker pulls
// when FORGE starts a managed Forgejo container.
func defaultImage() string {
	return defaultImageRepo + ":" + defaultImageTag + "@" + defaultImageDigest
}
