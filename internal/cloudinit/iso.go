package cloudinit

import (
	"bytes"
	"fmt"
	"os"

	"github.com/kdomanski/iso9660"
)

// WriteISO writes a NoCloud-compatible ISO9660 image to outPath. The image
// has volume label "cidata" and contains user-data, meta-data, and
// network-config files at the root. cloud-init in the guest auto-detects
// this and applies the configuration on first boot.
func WriteISO(outPath string, userData, metaData, networkConfig []byte) error {
	w, err := iso9660.NewWriter()
	if err != nil {
		return fmt.Errorf("creating ISO writer: %w", err)
	}
	defer func() { _ = w.Cleanup() }()

	if err := w.AddFile(bytes.NewReader(userData), "user-data"); err != nil {
		return fmt.Errorf("adding user-data: %w", err)
	}
	if err := w.AddFile(bytes.NewReader(metaData), "meta-data"); err != nil {
		return fmt.Errorf("adding meta-data: %w", err)
	}
	if err := w.AddFile(bytes.NewReader(networkConfig), "network-config"); err != nil {
		return fmt.Errorf("adding network-config: %w", err)
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() { _ = out.Close() }()

	if err := w.WriteTo(out, "cidata"); err != nil {
		return fmt.Errorf("writing ISO image: %w", err)
	}
	return nil
}
