// Package env contains the orchestration logic for the `forge env`
// commands. The cobra commands in cmd/env are thin wrappers around the
// functions in this package so the orchestration stays testable.
package env

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/p5n-dev/forge/internal/cloudinit"
	"github.com/p5n-dev/forge/internal/image"
	"github.com/p5n-dev/forge/internal/progress"
	"github.com/p5n-dev/forge/internal/vm"
)

// CreateInput is the user-facing configuration for `forge env create`.
type CreateInput struct {
	Name           string
	EnvBaseDir     string // ~/.forge/envs (already expanded)
	ImageCacheDir  string // ~/.forge/images (already expanded)
	ImageVersion   string // empty → use latest cached
	CPUs           int
	MemoryMB       int
	DiskMB         int
	K3sVersion     string
	RageVersion    string
	ClaudeVersion  string
	HelmVersion    string
	ForgejoBaseURL string // URL Forgejo lives at, used for both API and VM views (vsock-bridged inside the guest)
	ForgejoUser    string
	ForgejoToken   string
	// ForgejoProxyTarget is the host TCP endpoint the per-env unix
	// socket proxy bridges to. Typically "127.0.0.1:<port>" — the
	// Forgejo Docker container's published port on the macOS host.
	ForgejoProxyTarget string
	// ForgejoVsockPort is the vsock port shared end-to-end across the
	// in-VM socat unit, the vfkit virtio-vsock listen device, and the
	// in-VM TCP listener at 127.0.0.1:<port>. Conventionally the same
	// as the host Forgejo port so URLs match across the boundary.
	ForgejoVsockPort int
	// RageShareDir, when set, is shared into the guest as a read-only
	// virtio-fs mount. Cloud-init's forge-bootstrap copies the right
	// rage binary out of it. Empty → guest skips rage install.
	// Typically resolved from ~/.forge/rage by the cobra wrapper.
	RageShareDir string
	// HostUID is the numeric UID we set for the in-VM forge user, so
	// files in the workspace virtio-fs share have matching ownership
	// on both sides. The cobra wrapper passes os.Getuid().
	HostUID int
	// ProjectRootDir is the host directory where forge.yaml was
	// discovered (empty when running with built-in defaults). After
	// the workspace clone we copy a small set of project seed files
	// (.pre-commit-config.yaml today; extensible later) from here
	// into the workspace and push the seed commit so subsequent VM
	// boots clone a non-empty repo.
	ProjectRootDir string
}

// Validate ensures required fields are populated.
func (in CreateInput) Validate() error {
	if in.Name == "" {
		return fmt.Errorf("env name is required")
	}
	if in.EnvBaseDir == "" {
		return fmt.Errorf("env base dir is required")
	}
	if in.ImageCacheDir == "" {
		return fmt.Errorf("image cache dir is required")
	}
	if in.CPUs <= 0 || in.MemoryMB <= 0 || in.DiskMB <= 0 {
		return fmt.Errorf("cpus, memory, and disk must all be positive")
	}
	if in.K3sVersion == "" || in.RageVersion == "" || in.ClaudeVersion == "" || in.HelmVersion == "" {
		return fmt.Errorf("bootstrap versions are required (k3s, rage, claude_code, helm)")
	}
	return nil
}

// Deps groups the side-effecting collaborators of Create. Tests inject
// fakes for everything below; production callers use NewDefaultDeps.
type Deps struct {
	Runner           vm.Runner
	NetRunner        NetRunner
	ProxyRunner      ProxyRunner
	Forgejo          ForgejoClient
	GenerateKeyPair  func(privPath, pubPath, comment string) error
	PrepareDisk      func(srcGz, dstRaw string, sizeBytes int64) error
	WriteISO         func(outPath string, userData, metaData, networkConfig []byte) error
	NewVsockListener func(sockPath string) (VsockListener, error)
	GenerateMAC      func() string
	Now              func() time.Time
	// GitClone clones cloneURL into dstDir, authenticating once with
	// (user, token), and rewriting origin to a clean (token-less) URL
	// after the clone so the credential isn't persisted in .git/config.
	// On macOS hosts the user's git credential helper (Keychain by
	// default) handles future pushes from the host clone.
	GitClone func(ctx context.Context, cloneURL, dstDir, user, token string) error
	// SeedWorkspace copies a curated set of project seed files
	// (.pre-commit-config.yaml today) from projectRoot into
	// workspaceDir, then commits and pushes them so the freshly
	// created Forgejo repo has a real default branch instead of
	// being unborn. No-op when projectRoot is empty (forge.yaml
	// discovery returned built-in defaults).
	SeedWorkspace func(ctx context.Context, projectRoot, workspaceDir, cloneURL, user, token string) error
	// Progress reports per-step status to the user. nil → progress.Nop().
	Progress progress.Progress
}

// ProxyRunner abstracts spawning and reaping the host-side
// forgejoproxy subprocess. Production: a thin wrapper that re-execs
// `forge env _proxy …`. Tests: an in-memory stub.
type ProxyRunner interface {
	Start(ctx context.Context, envDir, target string) error
	Stop(ctx context.Context, envDir string) error
}

// NetRunner abstracts spawning and reaping the host-side gvproxy
// (userspace netstack) subprocess. Production: a thin wrapper that
// re-execs `forge env _net …`. Tests: an in-memory stub.
//
// gvproxy must be running BEFORE vfkit starts because vfkit's
// virtio-net,unixSocketPath device dials the socket at boot — if
// nothing is listening, the VM has no NIC.
type NetRunner interface {
	Start(ctx context.Context, envDir string) error
	Stop(ctx context.Context, envDir string) error
}

// ForgejoClient is the subset of the forgejo API the create flow needs.
type ForgejoClient interface {
	EnsureRepo(ctx context.Context, name string) (cloneURL string, err error)
}

// VsockListener is what env create receives from NewVsockListener.
// Production code wraps internal/vsock.Listener; tests provide fakes.
type VsockListener interface {
	SocketPath() string
	WaitReady(ctx context.Context) (*VsockMessage, error)
	Close() error
}

// VsockMessage mirrors internal/vsock.Message in this package's vocabulary
// so callers don't need to import internal/vsock just to satisfy the
// VsockListener interface.
type VsockMessage struct {
	IP string
}

// CreateResult is what Create returns on success.
type CreateResult struct {
	State *vm.State
	// CloneURL is the Forgejo repo URL ready for `git remote add origin`.
	CloneURL string
}

// Create boots a new env end-to-end: prepares the disk, generates SSH
// keys, renders cloud-init, ensures a Forgejo repo exists, starts the
// VM, and blocks until the VM signals boot-complete via vsock.
func Create(ctx context.Context, in CreateInput, deps Deps) (*CreateResult, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	prog := deps.Progress
	if prog == nil {
		prog = progress.Nop()
	}

	envDir := filepath.Join(in.EnvBaseDir, in.Name)
	if _, err := os.Stat(envDir); err == nil {
		return nil, fmt.Errorf("env %q already exists at %s", in.Name, envDir)
	}

	imageVersion, imagePath, err := resolveImage(in.ImageCacheDir, in.ImageVersion)
	if err != nil {
		return nil, err
	}

	// Allocate the static IP BEFORE the env dir exists so a freshly
	// allocated IP doesn't collide with the in-progress allocation
	// itself (collectUsedIPs reads sibling envs only — but state.json
	// for this env doesn't exist yet either way).
	ip, err := AllocateIP(in.EnvBaseDir)
	if err != nil {
		return nil, fmt.Errorf("allocating IP: %w", err)
	}

	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating env dir: %w", err)
	}

	done := prog.Step("Generating per-env SSH keypair")
	privKey := filepath.Join(envDir, "id_ed25519")
	pubKey := filepath.Join(envDir, "id_ed25519.pub")
	if err := deps.GenerateKeyPair(privKey, pubKey, "forge-env-"+in.Name); err != nil {
		done(err)
		return nil, fmt.Errorf("ssh keygen: %w", err)
	}
	pubBytes, err := os.ReadFile(pubKey)
	if err != nil {
		done(err)
		return nil, fmt.Errorf("reading public key: %w", err)
	}
	done(nil)

	done = prog.Step(fmt.Sprintf("Preparing disk image (%s, %d MB)", imageVersion, in.DiskMB))
	diskPath := filepath.Join(envDir, "disk.img")
	if err := deps.PrepareDisk(imagePath, diskPath, int64(in.DiskMB)*1024*1024); err != nil {
		done(err)
		return nil, fmt.Errorf("preparing disk: %w", err)
	}
	done(nil)

	done = prog.Step("Provisioning Forgejo repo")
	hostCloneURL, err := deps.Forgejo.EnsureRepo(ctx, in.Name)
	if err != nil {
		done(err)
		return nil, fmt.Errorf("forgejo ensure repo: %w", err)
	}
	// VM-side clone URL is now identical to the host one — the in-VM
	// socat unit makes localhost:<port> reach the host's Forgejo via
	// vsock, so no host-rewrite is needed.
	vmCloneURL := hostCloneURL
	done(nil)

	// Clone the (just-provisioned, empty) Forgejo repo into envDir/workspace
	// so the env has a checkout from the moment it boots. The same
	// directory is shared into the VM via virtio-fs so the in-VM agent
	// works against the same tree.
	done = prog.Step("Cloning workspace")
	workspaceDir := WorkspaceDir(envDir)
	if err := deps.GitClone(ctx, hostCloneURL, workspaceDir, in.ForgejoUser, in.ForgejoToken); err != nil {
		done(err)
		return nil, fmt.Errorf("cloning workspace: %w", err)
	}
	done(nil)

	// Seed the freshly cloned workspace with project files (e.g.
	// .pre-commit-config.yaml) and push so future env clones from
	// the same Forgejo repo land on a real branch with that file in
	// place. Skipped when ProjectRootDir is empty (built-in defaults
	// path) or when SeedWorkspace isn't wired (test deps).
	if in.ProjectRootDir != "" && deps.SeedWorkspace != nil {
		done = prog.Step("Seeding workspace")
		if err := deps.SeedWorkspace(ctx, in.ProjectRootDir, workspaceDir, hostCloneURL, in.ForgejoUser, in.ForgejoToken); err != nil {
			done(err)
			return nil, fmt.Errorf("seeding workspace: %w", err)
		}
		done(nil)
	}

	done = prog.Step("Rendering cloud-init")
	userData, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              in.Name,
		AuthorizedKey:     trimSpace(string(pubBytes)),
		K3sVersion:        in.K3sVersion,
		RageVersion:       in.RageVersion,
		ClaudeCodeVersion: in.ClaudeVersion,
		HelmVersion:       in.HelmVersion,
		ForgejoRemoteURL:  vmCloneURL,
		HostUID:           in.HostUID,
		ForgejoHostBase:   in.ForgejoBaseURL,
		ForgejoVsockPort:  in.ForgejoVsockPort,
		ForgejoUser:       in.ForgejoUser,
		ForgejoToken:      in.ForgejoToken,
		// Match the per-env Forgejo user (see internal/forgejo/repos.go:
		// EmailDomain) so commits from inside the VM attribute correctly.
		GitUserName:  in.Name,
		GitUserEmail: in.Name + "@forge.local",
	})
	if err != nil {
		done(err)
		return nil, fmt.Errorf("rendering user-data: %w", err)
	}
	metaData := cloudinit.RenderMetaData(in.Name)
	networkConfig, err := cloudinit.RenderNetworkConfig(cloudinit.NetworkConfigInput{
		Address: ip,
		Prefix:  NetworkPrefix,
		Gateway: NetworkGateway,
		DNS:     []string{NetworkDNSPrimary, NetworkDNSFallback},
	})
	if err != nil {
		done(err)
		return nil, fmt.Errorf("rendering network-config: %w", err)
	}
	isoPath := filepath.Join(envDir, "cloud-init.iso")
	if err := deps.WriteISO(isoPath, userData, metaData, networkConfig); err != nil {
		done(err)
		return nil, fmt.Errorf("writing cloud-init iso: %w", err)
	}
	done(nil)

	mac := deps.GenerateMAC()
	state := &vm.State{
		Name:         in.Name,
		Status:       vm.StatusCreating,
		MAC:          mac,
		IP:           ip,
		CreatedAt:    deps.Now(),
		ImageVersion: imageVersion,
		CPUs:         in.CPUs,
		Memory:       in.MemoryMB,
		Disk:         in.DiskMB,
	}
	if err := state.Save(envDir); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	done = prog.Step(fmt.Sprintf("Booting VM (%d vCPU, %d MB RAM)", in.CPUs, in.MemoryMB))
	sockPath := filepath.Join(envDir, "vsock.sock")
	lis, err := deps.NewVsockListener(sockPath)
	if err != nil {
		done(err)
		return nil, fmt.Errorf("opening vsock listener: %w", err)
	}
	defer func() { _ = lis.Close() }()

	// vfkit refuses to bind a vsock socketURL whose path already exists.
	// Nuke any leftover before handing the path to the runner.
	sshSockPath := SSHSocketPath(envDir)
	if err := os.Remove(sshSockPath); err != nil && !os.IsNotExist(err) {
		done(err)
		return nil, fmt.Errorf("removing stale ssh socket: %w", err)
	}

	startOpts := vm.StartOptions{
		DiskPath:          diskPath,
		CloudInitISO:      isoPath,
		CPUs:              in.CPUs,
		MemoryMB:          in.MemoryMB,
		MAC:               mac,
		NetSocketPath:     NetSocketPath(envDir),
		EFIVarStorePath:   filepath.Join(envDir, "efi-vars"),
		VsockSocketPath:   sockPath,
		VsockPort:         1234,
		SSHSocketPath:     sshSockPath,
		RageShareDir:      in.RageShareDir,
		WorkspaceShareDir: workspaceDir,
		ForgejoSocketPath: ForgejoSocketPath(envDir),
		ForgejoVsockPort:  in.ForgejoVsockPort,
	}

	// markCrashed flips the persisted state to `crashed` so a failed
	// boot doesn't leave the env stuck in `creating` (which makes
	// `forge env list` look like an operation is still in flight).
	// `forge env stop --force` and `forge env destroy` both accept
	// crashed envs, so the user can recover without --force flags.
	markCrashed := func() {
		state.Status = vm.StatusCrashed
		_ = state.Save(envDir)
	}

	// gvproxy MUST come up before vfkit — vfkit's virtio-net device
	// dials the unix socket at boot; if nothing is listening, the
	// VM gets no NIC. Tests that don't inject a NetRunner skip this
	// (they're exercising other code paths and don't need a real
	// network).
	if deps.NetRunner != nil {
		if err := deps.NetRunner.Start(ctx, envDir); err != nil {
			done(err)
			markCrashed()
			return nil, fmt.Errorf("starting gvproxy: %w", err)
		}
	}
	// Bind the forgejoproxy unix socket BEFORE vfkit so the in-VM
	// socat unit's first vsock dial finds a live host-side listener.
	// Skipping when the env doesn't use Forgejo keeps the test path
	// (no ProxyRunner injected) as a no-op.
	if deps.ProxyRunner != nil && in.ForgejoVsockPort != 0 && in.ForgejoProxyTarget != "" {
		if err := deps.ProxyRunner.Start(ctx, envDir, in.ForgejoProxyTarget); err != nil {
			done(err)
			if deps.NetRunner != nil {
				_ = deps.NetRunner.Stop(ctx, envDir)
			}
			markCrashed()
			return nil, fmt.Errorf("starting forgejo proxy: %w", err)
		}
	}
	if err := deps.Runner.Start(ctx, envDir, startOpts); err != nil {
		done(err)
		// Tear down the sister processes too, otherwise their orphan
		// unix sockets block the next create attempt.
		if deps.ProxyRunner != nil {
			_ = deps.ProxyRunner.Stop(ctx, envDir)
		}
		if deps.NetRunner != nil {
			_ = deps.NetRunner.Stop(ctx, envDir)
		}
		markCrashed()
		return nil, fmt.Errorf("starting vfkit: %w", err)
	}
	state.Status = vm.StatusStarting
	if err := state.Save(envDir); err != nil {
		done(err)
		return nil, fmt.Errorf("saving state: %w", err)
	}
	done(nil)

	done = prog.Step("Bootstrapping VM (k3s, RAGE, Claude Code) — this can take a few minutes")
	if _, err := lis.WaitReady(ctx); err != nil {
		done(err)
		// Reap everything so the user can `forge env destroy` cleanly
		// and try again without --force on the next attempt.
		_ = deps.Runner.Stop(ctx, envDir)
		if deps.ProxyRunner != nil {
			_ = deps.ProxyRunner.Stop(ctx, envDir)
		}
		if deps.NetRunner != nil {
			_ = deps.NetRunner.Stop(ctx, envDir)
		}
		markCrashed()
		return nil, fmt.Errorf("waiting for vsock ready: %w", err)
	}
	state.Status = vm.StatusRunning
	if err := state.Save(envDir); err != nil {
		done(err)
		return nil, fmt.Errorf("saving state: %w", err)
	}
	done(nil)

	return &CreateResult{State: state, CloneURL: vmCloneURL}, nil
}

// resolveImage finds the image to use under cacheDir. If version is empty
// it picks the most recently pulled image. Returns (version, fullPath).
func resolveImage(cacheDir, version string) (string, string, error) {
	cached, err := image.ListCached(cacheDir)
	if err != nil {
		return "", "", fmt.Errorf("listing cached images: %w", err)
	}
	if len(cached) == 0 {
		return "", "", fmt.Errorf("no images cached in %s — run `forge image pull` first", cacheDir)
	}

	if version != "" {
		for _, c := range cached {
			if c.Version == version {
				return c.Version, c.Path, nil
			}
		}
		return "", "", fmt.Errorf("image version %s not in cache", version)
	}

	sort.Slice(cached, func(i, j int) bool {
		return cached[i].PulledAt.After(cached[j].PulledAt)
	})
	return cached[0].Version, cached[0].Path, nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
