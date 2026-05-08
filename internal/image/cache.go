package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CachedImage describes a base image present in the local cache.
type CachedImage struct {
	Version  string
	Path     string
	Size     int64
	PulledAt time.Time
}

// ExpandPath resolves a leading "~" to the user's home directory. Returns an
// empty string unchanged so callers can detect "unset" cleanly.
func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// CachePath returns the on-disk path of a cached image artifact for the given
// version, without checking whether the file actually exists.
func CachePath(cacheDir, version string) string {
	return filepath.Join(cacheDir, ImageAssetName(version))
}

// IsCached reports whether the image for version is already present in
// cacheDir. Returns false on any I/O error (the caller can re-pull).
func IsCached(cacheDir, version string) bool {
	info, err := os.Stat(CachePath(cacheDir, version))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// ListCached enumerates base images in cacheDir and returns them sorted by
// version, newest first. A nonexistent cacheDir is treated as empty rather
// than as an error — the cache hasn't been populated yet, that's fine.
func ListCached(cacheDir string) ([]CachedImage, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading cache directory %s: %w", cacheDir, err)
	}

	var images []CachedImage
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		version, ok := parseImageFilename(name)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", name, err)
		}
		images = append(images, CachedImage{
			Version:  version,
			Path:     filepath.Join(cacheDir, name),
			Size:     info.Size(),
			PulledAt: info.ModTime(),
		})
	}

	sort.Slice(images, func(i, j int) bool {
		return images[i].Version > images[j].Version
	})
	return images, nil
}

// parseImageFilename extracts the version from a cached image filename.
// Returns (version, true) when name matches forge-base-<version>-arm64.img.gz.
func parseImageFilename(name string) (string, bool) {
	const prefix = "forge-base-"
	const suffix = "-arm64.img.gz"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", false
	}
	version := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if version == "" {
		return "", false
	}
	return version, true
}
