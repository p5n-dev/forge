package image

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/image"
)

var (
	importFlagVersion string
	importFlagSBOM    string
	importFlagForce   bool
)

var importCmd = &cobra.Command{
	Use:   "import <path>",
	Short: "Import a locally-built or otherwise-obtained FORGE base image",
	Long: `Copies an existing .img.gz into ~/.forge/images/ so it can be used
by ` + "`forge env create`" + ` without going through the published release flow.

Use cases:
  - Local development: build the image with images/base/build.sh (or via
    the Docker wrapper) and import the result for immediate testing.
  - Sharing: a teammate hands you an image over scp/Airdrop/USB; import it.
  - Pinning: drop a known-good image onto an offline host without internet
    access to GitHub Releases.

The version is inferred from the source filename when it matches the
canonical 'forge-base-<version>-arm64.img.gz' pattern; pass --version to
override or to import a file with a different name.

Any SBOM (CycloneDX) sitting next to the source — either a versioned
'<name>.sbom.cdx.json' or the build-output 'sbom.cdx.json' — is copied
alongside automatically. Pass --sbom to point at one explicitly.`,
	Args: cobra.ExactArgs(1),
	RunE: runImport,
}

func init() {
	importCmd.Flags().StringVar(&importFlagVersion, "version", "", "version label (default: derive from filename)")
	importCmd.Flags().StringVar(&importFlagSBOM, "sbom", "", "explicit path to CycloneDX SBOM (default: auto-detect)")
	importCmd.Flags().BoolVar(&importFlagForce, "force", false, "overwrite an already-cached image of the same version")
	Cmd.AddCommand(importCmd)
}

func runImport(cmd *cobra.Command, args []string) error {
	src := args[0]
	cacheDir, err := resolveCacheDir()
	if err != nil {
		return err
	}

	res, err := image.Import(src, image.ImportOptions{
		CacheDir: cacheDir,
		Version:  importFlagVersion,
		SBOMPath: importFlagSBOM,
		Force:    importFlagForce,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printImportResult(out, res)
	return nil
}

func printImportResult(out io.Writer, res *image.ImportResult) {
	_, _ = fmt.Fprintln(out, successStyle.Render("✓ imported "+res.Version))
	_, _ = fmt.Fprintln(out, dimStyle.Render("  image: "+res.ImagePath))
	if res.SBOMPath != "" {
		_, _ = fmt.Fprintln(out, dimStyle.Render("  sbom:  "+res.SBOMPath))
	} else {
		_, _ = fmt.Fprintln(out, dimStyle.Render("  sbom:  (none)"))
	}
	_, _ = fmt.Fprintln(out, infoStyle.Render("Run `forge image list` to see all cached images."))
}
