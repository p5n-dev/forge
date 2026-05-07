package image

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultGitHubOwner is the default repository owner for FORGE base
	// images. Configurable so forks can self-host.
	DefaultGitHubOwner = "schubergphilis"

	// DefaultGitHubRepo is the default repository name.
	DefaultGitHubRepo = "forge"

	// DefaultGitHubBaseURL is the GitHub REST API root.
	DefaultGitHubBaseURL = "https://api.github.com"

	// SHA256SUMSAssetName is the canonical name of the published checksums
	// file. The same name appears inside SHA256SUMS lines next to each digest.
	SHA256SUMSAssetName = "SHA256SUMS"

	// PublishedSBOMAssetName is the CycloneDX SBOM asset name as published on
	// GitHub Releases. We rename it on download so multiple cached versions
	// don't clobber each other.
	PublishedSBOMAssetName = "sbom.cdx.json"
)

// GitHubReleasesSource pulls FORGE base images from a project's GitHub
// Releases page. It uses the public REST API directly (no SDK) to keep the
// dependency surface small.
type GitHubReleasesSource struct {
	owner   string
	repo    string
	baseURL string
	client  *http.Client
}

// Option customises a GitHubReleasesSource. Used by tests to point at an
// httptest server, but also useful for self-hosted GitHub Enterprise.
type Option func(*GitHubReleasesSource)

// WithBaseURL overrides the GitHub API root.
func WithBaseURL(url string) Option {
	return func(s *GitHubReleasesSource) {
		s.baseURL = strings.TrimRight(url, "/")
	}
}

// WithHTTPClient overrides the underlying HTTP client (e.g. to inject a token
// transport or shorter timeouts).
func WithHTTPClient(c *http.Client) Option {
	return func(s *GitHubReleasesSource) {
		s.client = c
	}
}

// NewGitHubReleasesSource constructs a GitHubReleasesSource. Empty owner/repo
// fall back to the FORGE defaults so callers reading from config don't need to
// special-case unset values.
func NewGitHubReleasesSource(owner, repo string, opts ...Option) *GitHubReleasesSource {
	if owner == "" {
		owner = DefaultGitHubOwner
	}
	if repo == "" {
		repo = DefaultGitHubRepo
	}
	src := &GitHubReleasesSource{
		owner:   owner,
		repo:    repo,
		baseURL: DefaultGitHubBaseURL,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
	for _, opt := range opts {
		opt(src)
	}
	return src
}

// Repo returns the configured (owner, repo) pair. Exposed for diagnostics and
// tests.
func (s *GitHubReleasesSource) Repo() (string, string) {
	return s.owner, s.repo
}

type ghAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// LatestVersion returns the tag of the most recent release.
func (s *GitHubReleasesSource) LatestVersion(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", s.baseURL, s.owner, s.repo)
	var rel ghRelease
	if err := s.getJSON(ctx, url, &rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("latest release has no tag_name")
	}
	return rel.TagName, nil
}

// ListVersions returns all release tags, newest first (GitHub's default
// ordering).
func (s *GitHubReleasesSource) ListVersions(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases", s.baseURL, s.owner, s.repo)
	var rels []ghRelease
	if err := s.getJSON(ctx, url, &rels); err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(rels))
	for _, r := range rels {
		if r.TagName != "" {
			versions = append(versions, r.TagName)
		}
	}
	return versions, nil
}

// Pull downloads the base image, the CycloneDX SBOM, and the SHA256SUMS file
// for version into destPath, then verifies the image checksum. On verification
// failure the partially-written image is removed.
func (s *GitHubReleasesSource) Pull(ctx context.Context, version, destPath string) error {
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return fmt.Errorf("creating destination %s: %w", destPath, err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/tag/%s", s.baseURL, s.owner, s.repo, version)
	var rel ghRelease
	if err := s.getJSON(ctx, url, &rel); err != nil {
		return err
	}

	imageAsset, ok := findAsset(rel.Assets, ImageAssetName(version))
	if !ok {
		return fmt.Errorf("release %s has no asset %s", version, ImageAssetName(version))
	}
	sumsAsset, ok := findAsset(rel.Assets, SHA256SUMSAssetName)
	if !ok {
		return fmt.Errorf("release %s has no %s asset", version, SHA256SUMSAssetName)
	}

	// Download checksums first — fail fast before pulling the (large) image
	// if the manifest is missing or corrupt.
	sumsBytes, err := s.downloadBytes(ctx, sumsAsset.DownloadURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", SHA256SUMSAssetName, err)
	}
	expected, err := lookupChecksum(sumsBytes, ImageAssetName(version))
	if err != nil {
		return err
	}

	// Image — written with checksum verification.
	imagePath := filepath.Join(destPath, ImageAssetName(version))
	if err := s.downloadVerified(ctx, imageAsset.DownloadURL, imagePath, expected); err != nil {
		_ = os.Remove(imagePath)
		return err
	}

	// SBOM is a nice-to-have alongside the image; treat its absence as an
	// error per the spec ("downloads the accompanying SBOM").
	if sbom, ok := findAsset(rel.Assets, PublishedSBOMAssetName); ok {
		sbomPath := filepath.Join(destPath, SBOMAssetName(version))
		if err := s.downloadFile(ctx, sbom.DownloadURL, sbomPath); err != nil {
			return fmt.Errorf("downloading SBOM: %w", err)
		}
	} else {
		return fmt.Errorf("release %s has no %s asset", version, PublishedSBOMAssetName)
	}

	// Persist the SHA256SUMS file too, so consumers can re-verify offline.
	if err := os.WriteFile(filepath.Join(destPath, SHA256SUMSAssetName), sumsBytes, 0o644); err != nil {
		return fmt.Errorf("writing SHA256SUMS: %w", err)
	}

	return nil
}

func findAsset(assets []ghAsset, name string) (ghAsset, bool) {
	for _, a := range assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// lookupChecksum parses a SHA256SUMS file (one "<hex>  <name>" per line) and
// returns the digest for filename. Lines may be in either coreutils format
// ("hex  name", two spaces) or BSD-style; we accept any whitespace between the
// two fields.
func lookupChecksum(sums []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		// Strip a leading "*" that GNU coreutils adds for binary mode.
		name := strings.TrimPrefix(fields[1], "*")
		if name == filename {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", filename)
}

// getJSON fetches url and decodes the body into out. Non-2xx responses are
// returned as errors that include the status code so callers can recognise
// 404 etc.
func (s *GitHubReleasesSource) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", url, err)
	}
	return nil
}

// downloadBytes pulls a small asset entirely into memory.
func (s *GitHubReleasesSource) downloadBytes(ctx context.Context, url string) ([]byte, error) {
	body, err := s.openDownload(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return io.ReadAll(body)
}

// downloadFile streams an asset to disk.
func (s *GitHubReleasesSource) downloadFile(ctx context.Context, url, dest string) (err error) {
	body, err := s.openDownload(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing %s: %w", dest, cerr)
		}
	}()

	if _, err := io.Copy(f, body); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return nil
}

// downloadVerified streams an asset to disk while computing its SHA256 in the
// same pass, and fails the download if the digest doesn't match expected.
func (s *GitHubReleasesSource) downloadVerified(ctx context.Context, url, dest, expected string) (err error) {
	body, err := s.openDownload(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing %s: %w", dest, cerr)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), body); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s",
			filepath.Base(dest), expected, got)
	}
	return nil
}

// openDownload performs a GET and returns the response body for the caller to
// consume. Callers must Close it.
func (s *GitHubReleasesSource) openDownload(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return resp.Body, nil
}

// Compile-time check that *GitHubReleasesSource satisfies ImageSource. Keep
// this here so a future signature change to ImageSource breaks the build at
// the implementation site rather than at every caller.
var _ ImageSource = (*GitHubReleasesSource)(nil)
