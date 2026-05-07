package forgejo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/forgejo"
)

// fakeRunner records each invocation and replies with scripted outputs / errors.
type fakeRunner struct {
	calls    [][]string
	stdouts  map[string]string
	exitErrs map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		stdouts:  map[string]string{},
		exitErrs: map[string]error{},
	}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	f.calls = append(f.calls, full)
	key := strings.Join(full, " ")
	if err, ok := f.exitErrs[key]; ok {
		return []byte(f.stdouts[key]), err
	}
	return []byte(f.stdouts[key]), nil
}

func (f *fakeRunner) findCall(prefix ...string) ([]string, bool) {
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if c[i] != p {
				match = false
				break
			}
		}
		if match {
			return c, true
		}
	}
	return nil, false
}

func TestManager_Start_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost ||
			!strings.HasSuffix(r.URL.Path, "/api/v1/users/forge/tokens") {
			t.Logf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Verify basic auth carries the password the user just set.
		gotUser, gotPwd, ok := r.BasicAuth()
		if !ok || gotUser != "forge" || gotPwd != "sup3rSecret!" {
			t.Errorf("unexpected basic auth: user=%q ok=%v", gotUser, ok)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sha1":"tok-abc-123"}`))
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	runner := newFakeRunner()
	// docker inspect returns non-zero (not running) so Start proceeds.
	runner.exitErrs["docker inspect -f {{.State.Running}} forge-forgejo"] = errors.New("not found")
	// Admin-user-create now runs with explicit --password and --email.
	createCmd := "docker exec forge-forgejo forgejo admin user create " +
		"--admin --username forge --password sup3rSecret! --email forge@forge.local"
	runner.stdouts[createCmd] = "" // no output needed; we no longer parse one

	mgr := forgejo.NewManager(forgejo.Options{
		Runner:  runner,
		DataDir: "/tmp/forge/forgejo",
		Port:    port, // matches httptest server so token-gen URL resolves correctly
		Image:   "codeberg.org/forgejo/forgejo:latest",
		Reachable: func(ctx context.Context, url string) (bool, string) {
			return true, "200 OK"
		},
	})

	creds, err := mgr.Start(context.Background(), forgejo.AdminCredentials{
		Username: "forge",
		Password: "sup3rSecret!",
	})
	require.NoError(t, err)
	assert.Equal(t, "forge", creds.Username)
	assert.Equal(t, "sup3rSecret!", creds.Password)
	assert.Equal(t, "tok-abc-123", creds.Token)

	call, ok := runner.findCall("docker", "run")
	require.True(t, ok, "docker run was not invoked: %v", runner.calls)
	got := strings.Join(call, " ")
	assert.Contains(t, got, "-d")
	assert.Contains(t, got, "--name forge-forgejo")
	assert.Contains(t, got, "-v /tmp/forge/forgejo:/data")
	assert.Contains(t, got, "codeberg.org/forgejo/forgejo:latest")
}

func TestManager_Start_RequiresPassword(t *testing.T) {
	mgr := forgejo.NewManager(forgejo.Options{Runner: newFakeRunner()})
	_, err := mgr.Start(context.Background(), forgejo.AdminCredentials{Username: "forge"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "password")
}

// portFromURL extracts the port from a URL produced by httptest.NewServer.
func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := neturl.Parse(raw)
	require.NoError(t, err)
	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return p
}

func TestManager_Start_PortInUse_ReturnsHelpfulError(t *testing.T) {
	runner := newFakeRunner()
	runner.exitErrs["docker inspect -f {{.State.Running}} forge-forgejo"] = errors.New("not found")
	dockerErr := "docker: Error response from daemon: driver failed programming external connectivity on endpoint forge-forgejo: Bind for 0.0.0.0:3000 failed: port is already allocated."
	startKey := "docker run -d --name forge-forgejo -p 3000:3000 -v /tmp/forge/forgejo:/data codeberg.org/forgejo/forgejo:latest"
	runner.stdouts[startKey] = dockerErr
	runner.exitErrs[startKey] = errors.New("exit status 125")

	mgr := forgejo.NewManager(forgejo.Options{
		Runner:    runner,
		DataDir:   "/tmp/forge/forgejo",
		Port:      3000,
		Image:     "codeberg.org/forgejo/forgejo:latest",
		Reachable: func(ctx context.Context, url string) (bool, string) { return true, "" },
	})

	_, err := mgr.Start(context.Background(), forgejo.AdminCredentials{Username: "forge", Password: "x"})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "3000", "error must call out the contended port")
	// Surface the underlying docker output so users can see what happened.
	assert.Contains(t, msg, "port is already allocated")
	// Steer them at the two real fixes — both expressed in YAML form
	// the user can paste straight into ~/.forge/config.yaml.
	assert.Contains(t, msg, "port:", "should mention the port config knob")
	assert.Contains(t, msg, "url:", "should mention the external-Forgejo escape hatch")
}

func TestManager_Start_GenericDockerError_IncludesOutput(t *testing.T) {
	runner := newFakeRunner()
	runner.exitErrs["docker inspect -f {{.State.Running}} forge-forgejo"] = errors.New("not found")
	startKey := "docker run -d --name forge-forgejo -p 3000:3000 -v /tmp/forge/forgejo:/data codeberg.org/forgejo/forgejo:latest"
	runner.stdouts[startKey] = "docker: Error response from daemon: pull access denied for foo."
	runner.exitErrs[startKey] = errors.New("exit status 125")

	mgr := forgejo.NewManager(forgejo.Options{
		Runner:    runner,
		DataDir:   "/tmp/forge/forgejo",
		Port:      3000,
		Image:     "codeberg.org/forgejo/forgejo:latest",
		Reachable: func(ctx context.Context, url string) (bool, string) { return true, "" },
	})

	_, err := mgr.Start(context.Background(), forgejo.AdminCredentials{Username: "forge", Password: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull access denied", "docker stderr must be surfaced")
}

func TestManager_Start_NoOpWhenAlreadyRunning(t *testing.T) {
	runner := newFakeRunner()
	// inspect returns "true" with no error => already running
	runner.stdouts["docker inspect -f {{.State.Running}} forge-forgejo"] = "true\n"

	mgr := forgejo.NewManager(forgejo.Options{
		Runner:    runner,
		DataDir:   "/tmp/forge/forgejo",
		Port:      3000,
		Image:     "codeberg.org/forgejo/forgejo:latest",
		Reachable: func(ctx context.Context, url string) (bool, string) { return true, "200 OK" },
	})

	_, err := mgr.Start(context.Background(), forgejo.AdminCredentials{Username: "forge", Password: "x"})
	require.NoError(t, err)
	_, ranRun := runner.findCall("docker", "run")
	assert.False(t, ranRun, "docker run should not be invoked when container is already running")
}

func TestManager_Stop_RunsDockerStopAndRm(t *testing.T) {
	runner := newFakeRunner()
	runner.stdouts["docker inspect -f {{.State.Running}} forge-forgejo"] = "true\n"

	mgr := forgejo.NewManager(forgejo.Options{Runner: runner})

	require.NoError(t, mgr.Stop(context.Background()))

	_, stopped := runner.findCall("docker", "stop", "forge-forgejo")
	assert.True(t, stopped, "expected docker stop call: %v", runner.calls)
	_, removed := runner.findCall("docker", "rm", "forge-forgejo")
	assert.True(t, removed, "expected docker rm call: %v", runner.calls)
}

func TestManager_Stop_WhenNotRunning(t *testing.T) {
	runner := newFakeRunner()
	runner.exitErrs["docker inspect -f {{.State.Running}} forge-forgejo"] = errors.New("not found")

	mgr := forgejo.NewManager(forgejo.Options{Runner: runner})

	require.NoError(t, mgr.Stop(context.Background()))
	// Even if not running we still attempt rm to clean up stopped containers; stop is skipped.
	_, stopped := runner.findCall("docker", "stop", "forge-forgejo")
	assert.False(t, stopped, "docker stop should not be invoked when container is not running")
}

func TestManager_IsRunning_True(t *testing.T) {
	runner := newFakeRunner()
	runner.stdouts["docker inspect -f {{.State.Running}} forge-forgejo"] = "true\n"

	mgr := forgejo.NewManager(forgejo.Options{Runner: runner})
	running, err := mgr.IsRunning(context.Background())
	require.NoError(t, err)
	assert.True(t, running)
}

func TestManager_IsRunning_False(t *testing.T) {
	runner := newFakeRunner()
	runner.exitErrs["docker inspect -f {{.State.Running}} forge-forgejo"] = errors.New("no such object")

	mgr := forgejo.NewManager(forgejo.Options{Runner: runner})
	running, err := mgr.IsRunning(context.Background())
	require.NoError(t, err)
	assert.False(t, running)
}

func TestReachable_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ok, reason := forgejo.Reachable(context.Background(), srv.URL)
	assert.True(t, ok, "expected reachable, got reason=%q", reason)
}

func TestReachable_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ok, reason := forgejo.Reachable(context.Background(), srv.URL)
	assert.False(t, ok)
	assert.Contains(t, reason, "404")
}

func TestReachable_ConnectionError(t *testing.T) {
	// Use a bogus port that nothing should be listening on
	ok, reason := forgejo.Reachable(context.Background(), "http://127.0.0.1:1")
	assert.False(t, ok)
	assert.NotEmpty(t, reason)
}

func TestParseRandomPassword(t *testing.T) {
	cases := map[string]string{
		"new password is 'sup3rSecret!'":                       "sup3rSecret!",
		"User 'forge' has been created with password 'abc123'": "abc123",
		"Some output\nnew password is 'multi-line!'\nmore":     "multi-line!",
	}
	for in, want := range cases {
		got, err := forgejo.ParseRandomPassword(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, "input=%q", in)
	}
}

func TestParseRandomPassword_NotFound(t *testing.T) {
	_, err := forgejo.ParseRandomPassword("no quoted password here")
	require.Error(t, err)
}
