package image_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/image"
)

// fakeRelease describes one release served by the test server. The artifacts
// map associates a filename with raw bytes; the server computes a download URL
// for each one and exposes it through the JSON API.
type fakeRelease struct {
	tag       string
	artifacts map[string][]byte
}

// newGitHubServer returns an httptest.Server that mimics the subset of the
// GitHub Releases REST API we use, plus an asset download handler. Releases
// are returned newest first, matching the real API.
func newGitHubServer(t *testing.T, owner, repo string, releases []fakeRelease) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	releasePath := func(tag string) string {
		return fmt.Sprintf("/repos/%s/%s/releases/tags/%s", owner, repo, tag)
	}
	downloadPath := func(tag, name string) string {
		return fmt.Sprintf("/download/%s/%s/%s/%s", owner, repo, tag, name)
	}

	buildAssets := func(rel fakeRelease) string {
		var parts []string
		for name := range rel.artifacts {
			parts = append(parts, fmt.Sprintf(
				`{"name":%q,"browser_download_url":%q}`,
				name, srv.URL+downloadPath(rel.tag, name),
			))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}

	buildRelease := func(rel fakeRelease) string {
		return fmt.Sprintf(`{"tag_name":%q,"assets":%s}`, rel.tag, buildAssets(rel))
	}

	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/releases/latest", owner, repo),
		func(w http.ResponseWriter, _ *http.Request) {
			if len(releases) == 0 {
				http.NotFound(w, nil)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(buildRelease(releases[0])))
		})

	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/releases", owner, repo),
		func(w http.ResponseWriter, _ *http.Request) {
			parts := make([]string, 0, len(releases))
			for _, r := range releases {
				parts = append(parts, buildRelease(r))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
		})

	for _, rel := range releases {
		rel := rel
		// Per-tag endpoint used by Pull when fetching a specific version.
		mux.HandleFunc(releasePath(rel.tag),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(buildRelease(rel)))
			})

		for name, data := range rel.artifacts {
			data := data
			mux.HandleFunc(downloadPath(rel.tag, name),
				func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write(data)
				})
		}
	}

	return srv
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sumsFile(entries map[string]string) []byte {
	var b strings.Builder
	for name, sum := range entries {
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	return []byte(b.String())
}

func TestGitHubReleasesSource_LatestVersion(t *testing.T) {
	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{
		{tag: "v0.2.0"},
		{tag: "v0.1.0"},
	})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	got, err := src.LatestVersion(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "v0.2.0", got)
}

func TestGitHubReleasesSource_LatestVersion_NotFound(t *testing.T) {
	srv := newGitHubServer(t, "p5n-dev", "forge", nil)

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	_, err := src.LatestVersion(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestGitHubReleasesSource_ListVersions(t *testing.T) {
	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{
		{tag: "v0.3.0"},
		{tag: "v0.2.0"},
		{tag: "v0.1.0"},
	})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	got, err := src.ListVersions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"v0.3.0", "v0.2.0", "v0.1.0"}, got)
}

func TestGitHubReleasesSource_Pull_Success(t *testing.T) {
	imageData := []byte("fake compressed image bytes")
	sbomData := []byte(`{"bomFormat":"CycloneDX"}`)
	imageName := image.ImageAssetName("v0.1.0")
	sums := sumsFile(map[string]string{imageName: sha256Hex(imageData)})

	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{{
		tag: "v0.1.0",
		artifacts: map[string][]byte{
			imageName:        imageData,
			"sbom.cdx.json":  sbomData,
			"sbom.spdx.json": []byte("ignored"),
			"SHA256SUMS":     sums,
		},
	}})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	dest := t.TempDir()
	err := src.Pull(context.Background(), "v0.1.0", dest)
	require.NoError(t, err)

	gotImage, err := os.ReadFile(filepath.Join(dest, imageName))
	require.NoError(t, err)
	assert.Equal(t, imageData, gotImage)

	gotSBOM, err := os.ReadFile(filepath.Join(dest, image.SBOMAssetName("v0.1.0")))
	require.NoError(t, err)
	assert.Equal(t, sbomData, gotSBOM)
}

func TestGitHubReleasesSource_Pull_ChecksumMismatch(t *testing.T) {
	imageData := []byte("real bytes")
	imageName := image.ImageAssetName("v0.1.0")
	sums := sumsFile(map[string]string{imageName: sha256Hex([]byte("different"))})

	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{{
		tag: "v0.1.0",
		artifacts: map[string][]byte{
			imageName:       imageData,
			"sbom.cdx.json": []byte("{}"),
			"SHA256SUMS":    sums,
		},
	}})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	dest := t.TempDir()
	err := src.Pull(context.Background(), "v0.1.0", dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum")

	// On checksum failure we should not leave a half-written image behind.
	_, statErr := os.Stat(filepath.Join(dest, imageName))
	assert.True(t, os.IsNotExist(statErr), "image file should have been cleaned up")
}

func TestGitHubReleasesSource_Pull_VersionNotFound(t *testing.T) {
	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{
		{tag: "v0.1.0", artifacts: map[string][]byte{}},
	})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	err := src.Pull(context.Background(), "v9.9.9", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestGitHubReleasesSource_Pull_MissingChecksums(t *testing.T) {
	imageName := image.ImageAssetName("v0.1.0")
	srv := newGitHubServer(t, "p5n-dev", "forge", []fakeRelease{{
		tag: "v0.1.0",
		artifacts: map[string][]byte{
			imageName:       []byte("data"),
			"sbom.cdx.json": []byte("{}"),
			// No SHA256SUMS asset.
		},
	}})

	src := image.NewGitHubReleasesSource("p5n-dev", "forge",
		image.WithBaseURL(srv.URL))

	err := src.Pull(context.Background(), "v0.1.0", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SHA256SUMS")
}

func TestGitHubReleasesSource_DefaultRepo(t *testing.T) {
	src := image.NewGitHubReleasesSource("", "")
	owner, repo := src.Repo()
	assert.Equal(t, "p5n-dev", owner)
	assert.Equal(t, "forge", repo)
}
