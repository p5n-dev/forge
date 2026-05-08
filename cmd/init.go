package cmd

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/internal/config"
)

var initFlagForce bool

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Initialise a directory for use with FORGE",
	Long: `Writes a default forge.yaml into the given directory (or the current
directory if none is given), so that 'forge env create' and 'forge env start'
can be run from there without depending on the FORGE git repository.

If a 'rage/' directory exists alongside forge.yaml, 'forge init' also
copies it into ~/.forge/rage so every env this host creates picks up the
same RAGE binary and rage.toml. This mirrors the layout CAGE prescribes
(see docs/cage-README.md). The expected contents are the platform-
specific rage binary (e.g. rage-aarch64-linux) and rage.toml. Files are
copied verbatim, preserving names and permissions.

Refuses if forge.yaml already exists. Pass --force to overwrite both
forge.yaml AND ~/.forge/rage.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) == 1 {
			dir = args[0]
		}
		out := cmd.OutOrStdout()
		if err := runInit(out, dir, initFlagForce); err != nil {
			return err
		}
		return setupRageFromProject(out, dir, initFlagForce)
	},
}

func init() {
	initCmd.Flags().BoolVarP(&initFlagForce, "force", "f", false,
		"Overwrite an existing forge.yaml and ~/.forge/rage")
	rootCmd.AddCommand(initCmd)
}

func runInit(out io.Writer, dir string, force bool) error {
	abs, err := config.WriteDefaultProject(dir, force)
	if err != nil {
		// Surface the more helpful hint when the only problem is that
		// a file already exists.
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("%w (pass --force to overwrite)", err)
		}
		return err
	}
	_, _ = fmt.Fprintf(out, "Wrote %s.\n", abs)
	_, _ = fmt.Fprintln(out, "Edit it to pin bootstrap versions and resource defaults for this project.")
	return nil
}

// setupRageFromProject copies a project-local rage/ directory to
// ~/.forge/rage so every env created on this host picks up the same
// RAGE binary and config. Mirrors the layout CAGE prescribes (see
// docs/cage-README.md).
//
// The contract:
//   - No rage/ in projectDir → friendly note, no error.
//   - ~/.forge/rage already exists, no --force → warning, no overwrite,
//     no error.
//   - ~/.forge/rage exists with --force → recursively removed first,
//     then a fresh copy.
//
// Failures during the copy itself (permissions, disk space, etc.) ARE
// surfaced as errors — silently losing data here would be worse than
// failing the command.
func setupRageFromProject(out io.Writer, projectDir string, force bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("looking up home directory: %w", err)
	}
	src := filepath.Join(projectDir, "rage")
	dst := filepath.Join(home, ".forge", "rage")
	return copyRageDir(out, src, dst, force)
}

// copyRageDir is the testable core of setupRageFromProject. Tests call
// this with arbitrary src and dst paths so they don't need to clobber
// the user's real $HOME.
func copyRageDir(out io.Writer, src, dst string, force bool) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintf(out, "No rage/ directory at %s — skipping rage setup.\n", src)
			_, _ = fmt.Fprintln(out, "  Add the rage Linux binary + rage.toml in a 'rage/' subdirectory to enable RAGE in your envs.")
			return nil
		}
		return fmt.Errorf("statting %s: %w", src, err)
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}

	if _, err := os.Stat(dst); err == nil {
		if !force {
			_, _ = fmt.Fprintf(out, "%s already exists — skipping rage setup. Re-run 'forge init --force' to replace it.\n", dst)
			return nil
		}
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("removing existing %s: %w", dst, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("statting %s: %w", dst, err)
	}

	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("copying %s -> %s: %w", src, dst, err)
	}
	_, _ = fmt.Fprintf(out, "Copied %s -> %s.\n", src, dst)
	return nil
}

// copyTree recursively copies src into dst, preserving file permissions
// (which matters: the rage binary needs to keep its 0755 bit so the
// guest can exec it). Symlinks and special files are skipped — release
// archives shouldn't contain them and silently following them would be
// surprising.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			// Skip symlinks, device files, sockets.
			return nil
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		return copyErr
	}
	return out.Close()
}
