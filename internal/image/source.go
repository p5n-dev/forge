// Package image provides base-image distribution and local cache management
// for FORGE. Image distribution is abstracted behind the ImageSource interface
// so that the GitHub Releases backend (MVP) can be swapped for an OCI registry
// later without touching callers.
package image

import "context"

// ImageSource fetches FORGE base images from a remote backend.
//
// The MVP implementation is GitHubReleasesSource. A future OCIRegistrySource
// will satisfy the same interface, so callers (the cmd layer) can stay
// backend-agnostic.
type ImageSource interface {
	// LatestVersion returns the version tag of the most recent release
	// (e.g. "v0.1.0").
	LatestVersion(ctx context.Context) (string, error)

	// Pull downloads the base image (and any sidecar artifacts such as the
	// CycloneDX SBOM) for the given version into destPath. destPath is the
	// directory the artifacts are written to; the image filename follows the
	// pattern forge-base-<version>-arm64.img.gz. Pull verifies the SHA256
	// checksum against SHA256SUMS and returns an error on mismatch.
	Pull(ctx context.Context, version, destPath string) error

	// ListVersions returns all available versions, newest first.
	ListVersions(ctx context.Context) ([]string, error)
}

// ImageAssetName returns the canonical asset filename for a given version.
// Centralised so callers and the cache layer stay in sync.
func ImageAssetName(version string) string {
	return "forge-base-" + version + "-arm64.img.gz"
}

// SBOMAssetName returns the CycloneDX SBOM filename a base image is published
// alongside. The published artifact is generic (sbom.cdx.json); locally we
// store it under a version-qualified name so multiple cached versions don't
// clash.
func SBOMAssetName(version string) string {
	return "forge-base-" + version + "-arm64.sbom.cdx.json"
}
