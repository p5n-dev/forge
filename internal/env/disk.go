package env

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

// PrepareDisk decompresses srcGz (typically a forge-base-*.img.gz) into
// dstRaw (a raw disk image), then extends dstRaw to sizeBytes via sparse
// truncation. Cloud-init's `growpart` will expand the partition table on
// first boot to consume the new space.
func PrepareDisk(srcGz, dstRaw string, sizeBytes int64) error {
	in, err := os.Open(srcGz)
	if err != nil {
		return fmt.Errorf("opening source image: %w", err)
	}
	defer func() { _ = in.Close() }()

	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	out, err := os.OpenFile(dstRaw, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating dest image: %w", err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, gz); err != nil {
		_ = os.Remove(dstRaw)
		return fmt.Errorf("decompressing image: %w", err)
	}

	info, err := out.Stat()
	if err != nil {
		return fmt.Errorf("statting dest image: %w", err)
	}
	if info.Size() < sizeBytes {
		if err := out.Truncate(sizeBytes); err != nil {
			return fmt.Errorf("extending dest image: %w", err)
		}
	}
	return nil
}
