package image

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImportOptions controls Import.
type ImportOptions struct {
	// CacheDir is where the imported image lands; usually the value of
	// global config's `image.cache_dir`. Required.
	CacheDir string
	// Version is the version string baked into the destination filename.
	// If empty, Import infers it from the source filename when it matches
	// the canonical `forge-base-<version>-arm64.img.gz` pattern; if it
	// can't be inferred, Import returns an error.
	Version string
	// SBOMPath is the optional path to a CycloneDX SBOM to copy alongside
	// the image. When empty, Import looks for one next to the source under
	// either `<basename>.sbom.cdx.json` or the unversioned `sbom.cdx.json`.
	SBOMPath string
	// Force overwrites an existing cached image of the same version.
	Force bool
}

// ImportResult describes what Import wrote into the cache.
type ImportResult struct {
	Version   string
	ImagePath string
	SBOMPath  string // empty if no SBOM was copied
}

// Import copies a locally-built or otherwise-obtained base image into the
// FORGE image cache. Used both by `forge image import` and by local-build
// pipelines that produce an image outside the GitHub Releases flow.
func Import(srcPath string, opts ImportOptions) (*ImportResult, error) {
	if opts.CacheDir == "" {
		return nil, errors.New("image: cache directory is required")
	}

	if err := assertGzip(srcPath); err != nil {
		return nil, err
	}

	version, err := resolveImportVersion(srcPath, opts.Version)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("image: creating cache dir: %w", err)
	}

	destImage := filepath.Join(opts.CacheDir, ImageAssetName(version))
	if !opts.Force {
		if _, err := os.Stat(destImage); err == nil {
			return nil, fmt.Errorf("image: %s is already cached (use --force to overwrite)", version)
		}
	}

	if err := copyFileAtomic(srcPath, destImage); err != nil {
		return nil, fmt.Errorf("image: copying image: %w", err)
	}

	res := &ImportResult{Version: version, ImagePath: destImage}

	sbomSrc, err := resolveSBOMPath(srcPath, opts.SBOMPath)
	if err != nil {
		return nil, err
	}
	if sbomSrc != "" {
		destSBOM := filepath.Join(opts.CacheDir, SBOMAssetName(version))
		if err := copyFileAtomic(sbomSrc, destSBOM); err != nil {
			return nil, fmt.Errorf("image: copying SBOM: %w", err)
		}
		res.SBOMPath = destSBOM
	}

	return res, nil
}

// assertGzip reads the first two bytes of srcPath and verifies they match the
// gzip magic (\x1f\x8b). This catches obvious mistakes (raw .img, empty file,
// truncated downloads) without decompressing the whole image.
func assertGzip(srcPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("image: opening source: %w", err)
	}
	defer func() { _ = f.Close() }()

	var magic [2]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil || n != 2 {
		return fmt.Errorf("image: %s is too small to be a gzip stream", srcPath)
	}
	if magic[0] != 0x1f || magic[1] != 0x8b {
		return fmt.Errorf("image: %s is not gzip-compressed", srcPath)
	}
	return nil
}

// resolveImportVersion returns the explicit version when given, otherwise
// derives it from the source filename's canonical pattern.
func resolveImportVersion(srcPath, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	base := filepath.Base(srcPath)
	if v, ok := parseImageFilename(base); ok {
		return v, nil
	}
	return "", fmt.Errorf("image: cannot infer version from %q — pass --version", base)
}

// resolveSBOMPath returns the SBOM path to copy. Honours an explicit path,
// then tries a versioned sibling, then the generic build-output `sbom.cdx.json`.
// Returns ("", nil) when no SBOM is available — that's not an error.
func resolveSBOMPath(srcPath, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("image: SBOM not found at %s: %w", explicit, err)
		}
		return explicit, nil
	}

	dir := filepath.Dir(srcPath)
	base := filepath.Base(srcPath)
	versioned := filepath.Join(dir, strings.TrimSuffix(base, ".img.gz")+".sbom.cdx.json")
	if _, err := os.Stat(versioned); err == nil {
		return versioned, nil
	}
	generic := filepath.Join(dir, "sbom.cdx.json")
	if _, err := os.Stat(generic); err == nil {
		return generic, nil
	}
	return "", nil
}

// copyFileAtomic copies src to dst by writing to a sibling .tmp file first,
// then renaming. The destination is left untouched on copy failure.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
