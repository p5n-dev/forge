package env

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/p5n-dev/forge/internal/cloudinit"
	"github.com/p5n-dev/forge/internal/config"
	envpkg "github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/forgejo"
	"github.com/p5n-dev/forge/internal/progress"
	"github.com/p5n-dev/forge/internal/ssh"
	"github.com/p5n-dev/forge/internal/vm"
	"github.com/p5n-dev/forge/internal/vsock"
)

var (
	createFlagCPUs   int
	createFlagMemory int
	createFlagDisk   int
	createFlagImage  string
	createFlagNoInit bool
)

var createCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create and start a new FORGE environment",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		stdin := cmd.InOrStdin()
		return runCreate(cmd.Context(), stdin, os.Stdout, args[0], createFlagNoInit, isTerminal(stdin))
	},
}

func init() {
	createCmd.Flags().IntVar(&createFlagCPUs, "cpus", 0, "vCPU count (overrides forge.yaml defaults)")
	createCmd.Flags().IntVar(&createFlagMemory, "memory", 0, "RAM in MB (overrides forge.yaml defaults)")
	createCmd.Flags().IntVar(&createFlagDisk, "disk", 0, "Disk size in MB (overrides forge.yaml defaults)")
	createCmd.Flags().StringVar(&createFlagImage, "image", "", "Specific base image version (default: latest cached)")
	createCmd.Flags().BoolVar(&createFlagNoInit, "no-init", false,
		"Skip the 'no forge.yaml found' prompt and use built-in defaults")
	Cmd.AddCommand(createCmd)
}

func runCreate(ctx context.Context, stdin io.Reader, out *os.File, name string, noInit, interactive bool) error {
	proj, projSource, err := resolveProject(stdin, out, noInit, interactive)
	if err != nil {
		return err
	}
	// projectRoot is where forge.yaml was discovered (or "" when running
	// with built-in defaults). It's the source dir for any per-project
	// seed files (.pre-commit-config.yaml etc.) we copy into the env's
	// freshly cloned workspace.
	var projectRoot string
	if projSource != "" {
		projectRoot = filepath.Dir(projSource)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("looking up home directory: %w", err)
	}
	configPath := filepath.Join(home, ".forge", "config.yaml")
	global, err := config.LoadGlobal(configPath)
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	cpus := pickInt(createFlagCPUs, proj.Defaults.CPUs)
	memory := pickInt(createFlagMemory, proj.Defaults.Memory)
	disk := pickInt(createFlagDisk, proj.Defaults.Disk)

	base, proxyTarget, port := resolveForgejo(global)
	user, token := global.Forgejo.AdminUser, global.Forgejo.AdminToken
	if user == "" {
		user = "forge"
	}
	if global.Forgejo.URL != "" {
		token = global.Forgejo.Token
	}

	in := envpkg.CreateInput{
		Name:               name,
		EnvBaseDir:         filepath.Join(home, ".forge", "envs"),
		ImageCacheDir:      expandHome(home, global.Image.CacheDir),
		ImageVersion:       createFlagImage,
		CPUs:               cpus,
		MemoryMB:           memory,
		DiskMB:             disk,
		K3sVersion:         proj.Bootstrap.K3s,
		RageVersion:        proj.Bootstrap.Rage,
		ClaudeVersion:      proj.Bootstrap.ClaudeCode,
		HelmVersion:        proj.Bootstrap.Helm,
		ForgejoBaseURL:     base,
		ForgejoUser:        user,
		ForgejoToken:       token,
		ForgejoProxyTarget: proxyTarget,
		ForgejoVsockPort:   port,
		RageShareDir:       resolveRageShareDir(home),
		HostUID:            os.Getuid(),
		ProjectRootDir:     projectRoot,
	}

	deps := envpkg.Deps{
		Runner:           vm.NewVfkitRunner(),
		NetRunner:        newExecNetRunner(),
		ProxyRunner:      newExecProxyRunner(),
		Forgejo:          forgejo.NewAPIClient(base, user, token),
		GenerateKeyPair:  ssh.GenerateKeyPair,
		PrepareDisk:      envpkg.PrepareDisk,
		WriteISO:         cloudinit.WriteISO,
		NewVsockListener: newVsockAdapter,
		GenerateMAC:      vm.GenerateMAC,
		Now:              defaultNow,
		GitClone:         gitCloneWithToken,
		SeedWorkspace:    seedWorkspaceFromProject,
		Progress:         progress.Auto(out),
	}

	res, err := envpkg.Create(ctx, in, deps)
	if err != nil {
		return err
	}

	envDir := filepath.Join(in.EnvBaseDir, name)
	_, _ = fmt.Fprintf(out, "Environment %q is up.\n", name)
	_, _ = fmt.Fprintf(out, "  IP:        %s\n", res.State.IP)
	_, _ = fmt.Fprintf(out, "  SSH key:   %s\n", filepath.Join(envDir, "id_ed25519"))
	_, _ = fmt.Fprintf(out, "  Forgejo:   %s\n", res.CloneURL)
	_, _ = fmt.Fprintf(out, "\nConnect:\n  forge env connect %s\n", name)
	return nil
}

// logProjectSource records which forge.yaml (if any) was used so that
// `forge env create --debug` shows it. Empty source means built-in
// defaults — i.e. no forge.yaml was found anywhere up to the root.
func logProjectSource(source string) {
	if source == "" {
		log.Debug().Msg("no forge.yaml found; using built-in defaults (run `forge init` to customize)")
		return
	}
	log.Debug().Str("path", source).Msg("loaded forge.yaml")
}

// resolveProject finds a forge.yaml or — when none exists — either
// prompts to scaffold one (interactive=true) or uses embedded defaults
// (noInit=true). Non-interactive runs without --no-init fail with a hint
// so scripts don't silently inherit defaults the operator didn't see.
//
// interactive is decided by the caller (typically `isTerminal(stdin)`).
// Splitting the decision from the prompt keeps this function pure and
// unit-testable without a PTY.
func resolveProject(in io.Reader, out io.Writer, noInit, interactive bool) (config.ProjectConfig, string, error) {
	proj, source, err := config.Discover()
	if err != nil {
		return config.ProjectConfig{}, "", fmt.Errorf("loading forge.yaml: %w", err)
	}
	if source != "" || noInit {
		logProjectSource(source)
		return proj, source, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return config.ProjectConfig{}, "", fmt.Errorf("getting working directory: %w", err)
	}

	if !interactive {
		return config.ProjectConfig{}, "", fmt.Errorf(
			"no forge.yaml found in %s or any parent directory; "+
				"run `forge init` here, or pass --no-init to use built-in defaults",
			cwd)
	}

	_, _ = fmt.Fprintf(out, "No forge.yaml found in %s or any parent directory.\n", cwd)
	yes, err := promptYesNo(in, out, fmt.Sprintf("Initialize one in %s?", cwd), true)
	if err != nil {
		return config.ProjectConfig{}, "", err
	}
	if !yes {
		_, _ = fmt.Fprintln(out, "Continuing with built-in defaults.")
		logProjectSource("")
		return proj, "", nil
	}

	written, err := config.WriteDefaultProject(cwd, false)
	if err != nil {
		return config.ProjectConfig{}, "", fmt.Errorf("writing forge.yaml: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Wrote %s.\n", written)

	// Re-discover so the rest of the command sees the file we just
	// wrote (and so `--debug` reports the right source path).
	proj, source, err = config.Discover()
	if err != nil {
		return config.ProjectConfig{}, "", fmt.Errorf("loading forge.yaml: %w", err)
	}
	logProjectSource(source)
	return proj, source, nil
}

// promptYesNo asks a y/n question; defaultYes controls which answer
// the user gets by hitting Enter. Recognises y/yes/n/no, case-insensitive.
func promptYesNo(in io.Reader, out io.Writer, question string, defaultYes bool) (bool, error) {
	suffix := "[Y/n]"
	if !defaultYes {
		suffix = "[y/N]"
	}
	r := bufio.NewReader(in)
	for {
		_, _ = fmt.Fprintf(out, "%s %s: ", question, suffix)
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		// Unrecognised — re-prompt.
	}
}

// isTerminal reports whether r is the same file descriptor as the
// process's controlling terminal. Used to decide whether interactive
// prompts make sense (mirrors cmd/system/start.go's helper of the
// same name — kept duplicated rather than shared via a new package).
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func pickInt(flagVal, defaultVal int) int {
	if flagVal > 0 {
		return flagVal
	}
	return defaultVal
}

// resolveForgejo returns the Forgejo URL FORGE uses (both for the host
// API client AND for cloud-init — the VM reaches the same URL string
// because the in-VM forge-forgejo-vsock.service forwards 127.0.0.1:<port>
// over vsock to the host, where internal/forgejoproxy bridges into
// the actual Forgejo TCP endpoint). Also returns the TCP target the
// host-side proxy dials and the port to use on the vsock channel.
//
// The port number serves three purposes simultaneously: the in-VM
// loopback listener, the vsock channel, and the host TCP target. Using
// the same number throughout keeps URLs identical from inside and
// outside the VM, so `git remote -v` shows the same string in both
// views and no insteadOf rewrites are needed.
func resolveForgejo(cfg config.GlobalConfig) (base, proxyTarget string, port int) {
	if cfg.Forgejo.URL != "" {
		base = cfg.Forgejo.URL
		port = portFromURL(cfg.Forgejo.URL)
	} else {
		port = cfg.Forgejo.Port
		if port == 0 {
			port = forgejo.DefaultPort
		}
		base = fmt.Sprintf("http://localhost:%d", port)
	}
	// The host-side proxy always dials loopback — Forgejo runs in a
	// Docker container published on the host, and 127.0.0.1:<port> is
	// the loopback view of that publish.
	proxyTarget = fmt.Sprintf("127.0.0.1:%d", port)
	return
}

// portFromURL extracts the explicit port from rawURL, falling back to
// the scheme default (80 for http, 443 for https). Used when the user
// has configured a Forgejo URL and we need to know which port the
// host-side proxy should dial.
func portFromURL(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return forgejo.DefaultPort
	}
	if p := u.Port(); p != "" {
		var n int
		_, _ = fmt.Sscanf(p, "%d", &n)
		if n > 0 {
			return n
		}
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443
	}
	return 80
}

func expandHome(home, path string) string {
	if len(path) > 0 && path[0] == '~' {
		return filepath.Join(home, path[1:])
	}
	return path
}

// vsockAdapter wraps internal/vsock.Listener to satisfy env.VsockListener.
// The two types differ only in their Message type — internal/vsock owns
// its own type, and we copy the IP across to env.VsockMessage.
type vsockAdapter struct {
	inner *vsock.Listener
}

func newVsockAdapter(sockPath string) (envpkg.VsockListener, error) {
	lis, err := vsock.Listen(sockPath)
	if err != nil {
		return nil, err
	}
	return &vsockAdapter{inner: lis}, nil
}

func (a *vsockAdapter) SocketPath() string { return a.inner.SocketPath() }
func (a *vsockAdapter) Close() error       { return a.inner.Close() }
func (a *vsockAdapter) WaitReady(ctx context.Context) (*envpkg.VsockMessage, error) {
	msg, err := a.inner.WaitReady(ctx)
	if err != nil {
		return nil, err
	}
	return &envpkg.VsockMessage{IP: msg.IP}, nil
}

func defaultNow() time.Time { return time.Now() }

// resolveRageShareDir returns the host path to expose as the
// rage-share virtio-fs mount in the guest, or "" if the user hasn't
// run `forge init` with a rage/ directory yet. An empty string skips
// the share entirely — the env still boots, but `forge env connect`
// (without --bash) won't find rage in the guest. `forge env connect
// --bash` keeps working regardless.
func resolveRageShareDir(home string) string {
	dir := filepath.Join(home, ".forge", "rage")
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// gitCloneWithToken shells out to `git clone` with credentials embedded
// in the URL, then immediately rewrites origin to a clean URL so the
// token doesn't end up in `.git/config`. Future host pushes from the
// clone go through the user's normal git credential helper (Keychain
// on macOS); VM-side pushes use the in-VM credential.helper store
// configured by cloud-init.
func gitCloneWithToken(ctx context.Context, cloneURL, dstDir, user, token string) error {
	authURL, err := injectGitAuth(cloneURL, user, token)
	if err != nil {
		return fmt.Errorf("forming auth URL: %w", err)
	}
	cloneCmd := exec.CommandContext(ctx, "git", "clone", authURL, dstDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Strip the token from the persisted origin URL. If this fails the
	// clone is otherwise fine but a token would linger in .git/config —
	// blow away the workspace dir so the user's next `forge env create`
	// retry starts clean instead of inheriting tainted state.
	setURL := exec.CommandContext(ctx, "git", "-C", dstDir, "remote", "set-url", "origin", cloneURL)
	if out, err := setURL.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dstDir)
		return fmt.Errorf("git remote set-url: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// seedFiles is the curated list of files copied from the host project
// root into a fresh env's workspace. Keep this short — anything that's
// project-wide developer tooling (formatters, secret detection, lint
// configs) belongs here; per-component or build-output files do not.
var seedFiles = []string{
	".pre-commit-config.yaml",
}

// seedWorkspaceFromProject copies a small set of project-level dotfiles
// (currently just .pre-commit-config.yaml) from the host project root
// into the env's freshly cloned workspace, then commits and pushes. The
// push is what gives the new Forgejo repo a real default branch — the
// just-provisioned repo is otherwise unborn, and any subsequent VM-side
// clone would land in the same unborn-HEAD state.
//
// No-op when the project root has none of the seed files. Push uses an
// authenticated URL once and does not modify the persisted origin URL,
// matching gitCloneWithToken's hygiene.
func seedWorkspaceFromProject(ctx context.Context, projectRoot, workspaceDir, cloneURL, user, token string) error {
	var seeded []string
	for _, name := range seedFiles {
		src := filepath.Join(projectRoot, name)
		info, err := os.Stat(src)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("checking %s: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("reading %s: %w", src, err)
		}
		dst := filepath.Join(workspaceDir, name)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
		seeded = append(seeded, name)
	}
	if len(seeded) == 0 {
		return nil
	}

	addArgs := append([]string{"-C", workspaceDir, "add", "--"}, seeded...)
	if out, err := exec.CommandContext(ctx, "git", addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commit := exec.CommandContext(ctx, "git",
		"-C", workspaceDir,
		"-c", "user.name=forge",
		"-c", "user.email=forge@forge.local",
		"commit", "-m", "Seed project files from host (.pre-commit-config.yaml)",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	authURL, err := injectGitAuth(cloneURL, user, token)
	if err != nil {
		return fmt.Errorf("forming auth URL for push: %w", err)
	}
	// Push to the auth URL by argument so .git/config keeps the clean
	// origin URL set by gitCloneWithToken. HEAD:refs/heads/main makes
	// the freshly created Forgejo repo's default branch real.
	push := exec.CommandContext(ctx, "git",
		"-C", workspaceDir,
		"push", authURL, "HEAD:refs/heads/main",
	)
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// injectGitAuth returns rawURL with userinfo embedded, e.g.
// http://host:4000/repo.git + (forge, abc) → http://forge:abc@host:4000/repo.git.
func injectGitAuth(rawURL, user, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, token)
	return u.String(), nil
}
