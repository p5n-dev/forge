package env_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/env"
	"github.com/p5n-dev/forge/internal/vm"
)

func mustGzip(in []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(in)
	_ = gz.Close()
	return buf.Bytes()
}

// fakeRunner records the StartOptions but doesn't actually start anything.
type fakeRunner struct {
	startCalled bool
	gotEnvDir   string
	gotOpts     vm.StartOptions
	startErr    error
}

func (f *fakeRunner) Start(_ context.Context, envDir string, opts vm.StartOptions) error {
	f.startCalled = true
	f.gotEnvDir = envDir
	f.gotOpts = opts
	return f.startErr
}
func (f *fakeRunner) Stop(_ context.Context, _ string) error { return nil }
func (f *fakeRunner) IsAlive(_ string) (bool, error)         { return false, nil }

type fakeForgejo struct {
	gotName  string
	cloneURL string
	err      error
}

func (f *fakeForgejo) EnsureRepo(_ context.Context, name string) (string, error) {
	f.gotName = name
	return f.cloneURL, f.err
}

type fakeListener struct {
	path string
	ip   string
	err  error
}

func (f *fakeListener) SocketPath() string { return f.path }
func (f *fakeListener) Close() error       { return nil }
func (f *fakeListener) WaitReady(_ context.Context) (*env.VsockMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &env.VsockMessage{IP: f.ip}, nil
}

// helper to install a base image into a fake cache dir
func writeFakeImage(t *testing.T, cacheDir, version string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	imgPath := filepath.Join(cacheDir, "forge-base-"+version+"-arm64.img.gz")
	// Create a minimal valid gzip stream.
	require.NoError(t, os.WriteFile(imgPath, gzipBytes([]byte("dummy disk content")), 0o644))
}

func gzipBytes(in []byte) []byte {
	return mustGzip(in)
}

func defaultInput(t *testing.T, root string) env.CreateInput {
	t.Helper()
	envBase := filepath.Join(root, "envs")
	cacheDir := filepath.Join(root, "images")
	writeFakeImage(t, cacheDir, "0.1.0")

	return env.CreateInput{
		Name:               "myproj",
		EnvBaseDir:         envBase,
		ImageCacheDir:      cacheDir,
		CPUs:               2,
		MemoryMB:           4096,
		DiskMB:             128, // small for test
		K3sVersion:         "v1.32.0+k3s1",
		RageVersion:        "v0.4.2",
		ClaudeVersion:      "latest",
		HelmVersion:        "v3.20.2",
		ForgejoBaseURL:     "http://localhost:3000",
		ForgejoUser:        "forge",
		ForgejoToken:       "tok",
		ForgejoProxyTarget: "127.0.0.1:3000",
		ForgejoVsockPort:   3000,
	}
}

func defaultDeps(runner vm.Runner, fj env.ForgejoClient, lis env.VsockListener) env.Deps {
	return env.Deps{
		Runner:  runner,
		Forgejo: fj,
		GenerateKeyPair: func(privPath, pubPath, _ string) error {
			if err := os.WriteFile(privPath, []byte("PRIVKEY"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(pubPath, []byte("ssh-ed25519 AAAA forge-test\n"), 0o644)
		},
		PrepareDisk: func(_, dst string, size int64) error {
			return os.WriteFile(dst, make([]byte, size), 0o644)
		},
		WriteISO: func(out string, _, _, _ []byte) error {
			return os.WriteFile(out, []byte("ISO"), 0o644)
		},
		NewVsockListener: func(_ string) (env.VsockListener, error) { return lis, nil },
		GenerateMAC:      func() string { return "52:54:00:aa:bb:cc" },
		Now:              func() time.Time { return time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC) },
		// Fake clone: just create the destination dir + a sentinel file
		// so downstream code that stats workspaceDir succeeds. Real
		// production GitClone shells out to `git clone`.
		GitClone: func(_ context.Context, _, dst, _, _ string) error {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dst, ".git-fake"), []byte("test"), 0o644)
		},
	}
}

func TestCreate_HappyPath(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)

	runner := &fakeRunner{}
	fj := &fakeForgejo{cloneURL: "http://localhost:3000/forge/myproj.git"}
	lis := &fakeListener{path: "/tmp/vsock.sock", ip: "192.168.127.42"}

	res, err := env.Create(context.Background(), in, defaultDeps(runner, fj, lis))
	require.NoError(t, err)

	envDir := filepath.Join(in.EnvBaseDir, "myproj")
	assert.True(t, runner.startCalled)
	assert.Equal(t, envDir, runner.gotEnvDir)
	assert.Equal(t, 2, runner.gotOpts.CPUs)
	assert.Equal(t, 4096, runner.gotOpts.MemoryMB)
	assert.Equal(t, "52:54:00:aa:bb:cc", runner.gotOpts.MAC)
	assert.Equal(t, filepath.Join(envDir, "vsock.sock"), runner.gotOpts.VsockSocketPath)
	assert.Equal(t, filepath.Join(envDir, "ssh.sock"), runner.gotOpts.SSHSocketPath)
	assert.Equal(t, filepath.Join(envDir, "workspace"), runner.gotOpts.WorkspaceShareDir,
		"workspace dir must be virtio-fs-shared into the VM")
	assert.Equal(t, filepath.Join(envDir, "efi-vars"), runner.gotOpts.EFIVarStorePath)

	// State persisted with running + IP from vsock.
	state, err := vm.LoadState(envDir)
	require.NoError(t, err)
	assert.Equal(t, vm.StatusRunning, state.Status)
	assert.Equal(t, "192.168.127.42", state.IP)
	assert.Equal(t, "0.1.0", state.ImageVersion)

	// Forgejo called with right name; clone URL passes through unchanged
	// (the in-VM socat unit makes localhost:<port> reach the host's
	// Forgejo over vsock, so the URL string is the same in both views).
	assert.Equal(t, "myproj", fj.gotName)
	assert.Equal(t, "http://localhost:3000/forge/myproj.git", res.CloneURL)

	// Files exist on disk.
	for _, p := range []string{"id_ed25519", "id_ed25519.pub", "disk.img", "cloud-init.iso", "state.json"} {
		_, err := os.Stat(filepath.Join(envDir, p))
		assert.NoError(t, err, "missing %s", p)
	}
}

// TestCreate_SeedWorkspaceCalledWhenProjectRootSet verifies the wiring
// between CreateInput.ProjectRootDir and Deps.SeedWorkspace: when the
// caller passes a non-empty project root, Create must invoke the seed
// hook with the same projectRoot/workspaceDir/cloneURL/user/token. The
// production seed function lives in cmd/env (file-copy + git push)
// and is exercised by its own internal test.
func TestCreate_SeedWorkspaceCalledWhenProjectRootSet(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)
	in.ProjectRootDir = filepath.Join(root, "host-project")

	runner := &fakeRunner{}
	fj := &fakeForgejo{cloneURL: "http://localhost:3000/forge/myproj.git"}
	lis := &fakeListener{path: "/tmp/vsock.sock", ip: "192.168.127.42"}

	deps := defaultDeps(runner, fj, lis)

	var seedProjectRoot, seedWorkspace, seedURL, seedUser, seedToken string
	var seedCalled bool
	deps.SeedWorkspace = func(_ context.Context, projectRoot, workspaceDir, cloneURL, user, token string) error {
		seedCalled = true
		seedProjectRoot = projectRoot
		seedWorkspace = workspaceDir
		seedURL = cloneURL
		seedUser = user
		seedToken = token
		return nil
	}

	_, err := env.Create(context.Background(), in, deps)
	require.NoError(t, err)
	assert.True(t, seedCalled, "SeedWorkspace must run when ProjectRootDir is set")
	assert.Equal(t, in.ProjectRootDir, seedProjectRoot)
	assert.Equal(t, filepath.Join(in.EnvBaseDir, in.Name, "workspace"), seedWorkspace)
	assert.Equal(t, fj.cloneURL, seedURL)
	assert.Equal(t, in.ForgejoUser, seedUser)
	assert.Equal(t, in.ForgejoToken, seedToken)
}

func TestCreate_SeedWorkspaceSkippedWhenProjectRootEmpty(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)
	// ProjectRootDir intentionally not set — built-in defaults path.

	runner := &fakeRunner{}
	fj := &fakeForgejo{cloneURL: "http://localhost:3000/forge/myproj.git"}
	lis := &fakeListener{path: "/tmp/vsock.sock", ip: "192.168.127.42"}

	deps := defaultDeps(runner, fj, lis)
	seedCalled := false
	deps.SeedWorkspace = func(_ context.Context, _, _, _, _, _ string) error {
		seedCalled = true
		return nil
	}

	_, err := env.Create(context.Background(), in, deps)
	require.NoError(t, err)
	assert.False(t, seedCalled, "SeedWorkspace must NOT run when ProjectRootDir is empty")
}

func TestCreate_RejectsExistingEnv(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)
	require.NoError(t, os.MkdirAll(filepath.Join(in.EnvBaseDir, in.Name), 0o755))

	_, err := env.Create(context.Background(), in,
		defaultDeps(&fakeRunner{}, &fakeForgejo{}, &fakeListener{ip: "1.1.1.1"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCreate_NoCachedImage(t *testing.T) {
	root := t.TempDir()
	in := env.CreateInput{
		Name:           "x",
		EnvBaseDir:     filepath.Join(root, "envs"),
		ImageCacheDir:  filepath.Join(root, "images"),
		CPUs:           2,
		MemoryMB:       4096,
		DiskMB:         128,
		K3sVersion:     "v1",
		RageVersion:    "v1",
		ClaudeVersion:  "v1",
		HelmVersion:    "v3",
		ForgejoBaseURL: "http://localhost:3000",
	}
	require.NoError(t, os.MkdirAll(in.ImageCacheDir, 0o755))

	_, err := env.Create(context.Background(), in,
		defaultDeps(&fakeRunner{}, &fakeForgejo{}, &fakeListener{ip: "x"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no images cached")
}

func TestCreate_SpecificImageVersion(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)
	writeFakeImage(t, in.ImageCacheDir, "0.2.0")
	in.ImageVersion = "0.2.0"

	_, err := env.Create(context.Background(), in,
		defaultDeps(&fakeRunner{}, &fakeForgejo{cloneURL: "http://x/y.git"}, &fakeListener{ip: "1.1.1.1"}))
	require.NoError(t, err)

	state, err := vm.LoadState(filepath.Join(in.EnvBaseDir, in.Name))
	require.NoError(t, err)
	assert.Equal(t, "0.2.0", state.ImageVersion)
}

func TestCreate_VsockTimeoutPropagates(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)

	runner := &fakeRunner{}
	fj := &fakeForgejo{cloneURL: "http://x/y.git"}
	lis := &fakeListener{err: context.DeadlineExceeded}

	_, err := env.Create(context.Background(), in, defaultDeps(runner, fj, lis))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vsock ready")
}

func TestCreate_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*env.CreateInput)
	}{
		{"no name", func(in *env.CreateInput) { in.Name = "" }},
		{"no cpus", func(in *env.CreateInput) { in.CPUs = 0 }},
		{"no mem", func(in *env.CreateInput) { in.MemoryMB = 0 }},
		{"no disk", func(in *env.CreateInput) { in.DiskMB = 0 }},
		{"no k3s", func(in *env.CreateInput) { in.K3sVersion = "" }},
		{"no rage", func(in *env.CreateInput) { in.RageVersion = "" }},
		{"no claude", func(in *env.CreateInput) { in.ClaudeVersion = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			in := defaultInput(t, root)
			tc.mod(&in)
			_, err := env.Create(context.Background(), in,
				defaultDeps(&fakeRunner{}, &fakeForgejo{}, &fakeListener{ip: "x"}))
			require.Error(t, err)
		})
	}
}

// recordingProgress captures the sequence of Step descriptions and the
// success/failure of each so tests can pin Create's progress contract
// against drift (a future refactor that drops a step name should fail
// here, not silently).
type recordingProgress struct {
	starts []string
	dones  []error
}

func (p *recordingProgress) Step(desc string) func(error) {
	p.starts = append(p.starts, desc)
	return func(err error) { p.dones = append(p.dones, err) }
}

func TestCreate_ReportsProgressForEachPhase(t *testing.T) {
	root := t.TempDir()
	in := defaultInput(t, root)

	rec := &recordingProgress{}
	deps := defaultDeps(&fakeRunner{}, &fakeForgejo{cloneURL: "http://localhost:3000/forge/myproj.git"},
		&fakeListener{ip: "192.168.127.42"})
	deps.Progress = rec

	_, err := env.Create(context.Background(), in, deps)
	require.NoError(t, err)

	// One done() per Step() — i.e. every step is closed out.
	require.Equal(t, len(rec.starts), len(rec.dones), "every Step must be closed with a done() call")
	for i, e := range rec.dones {
		assert.NoError(t, e, "step %d (%q) reported an error", i, rec.starts[i])
	}

	// The descriptions are user-facing, so we keep the assertion to
	// substring matches: this lets us tweak wording without churning
	// the test, while still catching a missing phase.
	expectedSubstrings := []string{
		"SSH keypair",
		"disk image",
		"Forgejo repo",
		"workspace",
		"cloud-init",
		"Booting VM",
		"Bootstrapping VM",
	}
	require.GreaterOrEqual(t, len(rec.starts), len(expectedSubstrings),
		"got fewer steps than expected; have %v", rec.starts)
	for i, want := range expectedSubstrings {
		assert.Contains(t, rec.starts[i], want, "step %d should mention %q, got %q", i, want, rec.starts[i])
	}
}
