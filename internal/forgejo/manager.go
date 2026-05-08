// Package forgejo manages the lifecycle of the local Forgejo Docker container
// that FORGE uses as a Git review gate. The implementation shells out to the
// `docker` CLI to keep the dependency footprint small and to mirror the pattern
// FORGE already uses for other external binaries (e.g. vfkit).
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// DefaultContainerName is the docker container name FORGE uses for Forgejo.
const DefaultContainerName = "forge-forgejo"

// DefaultPort is the host port Forgejo is exposed on by default.
const DefaultPort = 3000

// CommandRunner is a thin shim over os/exec so tests can mock the docker CLI.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ReachableFunc reports whether a Forgejo URL is reachable. It is exposed as
// an option so tests can stub HTTP without a real server.
type ReachableFunc func(ctx context.Context, url string) (bool, string)

// AdminCredentials is what `forge system start` provides up-front so the
// admin user is created with known credentials. The user authenticates to
// the Forgejo web UI with Username + Password; FORGE itself uses Token
// for API calls (created automatically after the admin user is provisioned).
type AdminCredentials struct {
	Username string
	Password string
}

// Credentials is the per-Forgejo admin identity Manager.Start creates.
// Username + Password are what the human user enters at start time and
// uses to log into the Forgejo UI. Token is an API token FORGE generates
// for its own automation; only Username + Token end up persisted to
// ~/.forge/config.yaml — the password stays the user's responsibility.
type Credentials struct {
	Username string
	Password string
	Token    string
}

// Options configure a Manager.
type Options struct {
	Runner        CommandRunner
	Reachable     ReachableFunc
	ContainerName string
	Image         string
	DataDir       string
	Port          int
	AdminUsername string
	// ReadyTimeout caps how long Start waits for Forgejo to become reachable.
	ReadyTimeout time.Duration
}

// Manager wraps the docker container lifecycle for Forgejo.
type Manager struct {
	runner        CommandRunner
	reachable     ReachableFunc
	containerName string
	image         string
	dataDir       string
	port          int
	adminUser     string
	readyTimeout  time.Duration
}

// NewManager builds a Manager. Sensible defaults are filled in for any
// zero-valued options.
func NewManager(opts Options) *Manager {
	m := &Manager{
		runner:        opts.Runner,
		reachable:     opts.Reachable,
		containerName: opts.ContainerName,
		image:         opts.Image,
		dataDir:       opts.DataDir,
		port:          opts.Port,
		adminUser:     opts.AdminUsername,
		readyTimeout:  opts.ReadyTimeout,
	}
	if m.runner == nil {
		m.runner = ExecRunner{}
	}
	if m.reachable == nil {
		m.reachable = Reachable
	}
	if m.containerName == "" {
		m.containerName = DefaultContainerName
	}
	if m.image == "" {
		m.image = defaultImage()
	}
	if m.port == 0 {
		m.port = DefaultPort
	}
	if m.adminUser == "" {
		m.adminUser = "forge"
	}
	if m.readyTimeout == 0 {
		m.readyTimeout = 60 * time.Second
	}
	return m
}

// URL returns the http URL the manager exposes Forgejo on (host-perspective).
func (m *Manager) URL() string {
	return fmt.Sprintf("http://localhost:%d", m.port)
}

// ContainerName returns the docker container name in use.
func (m *Manager) ContainerName() string { return m.containerName }

// Port returns the host port mapping in use.
func (m *Manager) Port() int { return m.port }

// IsRunning returns true if the Forgejo container exists and is running.
func (m *Manager) IsRunning(ctx context.Context) (bool, error) {
	out, err := m.runner.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", m.containerName)
	if err != nil {
		// docker inspect returns non-zero when the container does not exist.
		// We treat that as "not running" rather than a hard error so callers
		// can use IsRunning as a simple boolean.
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// Start launches the Forgejo container if it is not already running, waits for
// it to become reachable and bootstraps an admin user. It returns the admin
// credentials. If the container is already running, no admin user is created
// and Credentials is returned empty.
// Start launches the Forgejo container if it is not already running, waits
// for it to become reachable, creates the admin user with the supplied
// credentials, and generates an API token FORGE will use for subsequent
// admin work.
//
// If the container is already running, no admin user is created and an
// empty Credentials is returned — the caller is expected to already have
// credentials persisted in ~/.forge/config.yaml.
func (m *Manager) Start(ctx context.Context, in AdminCredentials) (Credentials, error) {
	if in.Username == "" {
		in.Username = m.adminUser
	}
	if in.Password == "" {
		return Credentials{}, fmt.Errorf("admin password is required")
	}

	running, err := m.IsRunning(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("checking forgejo state: %w", err)
	}
	if running {
		return Credentials{}, nil
	}

	args := []string{
		"run", "-d",
		"--name", m.containerName,
		"-p", fmt.Sprintf("%d:3000", m.port),
		"-v", fmt.Sprintf("%s:/data", m.dataDir),
		m.image,
	}
	if out, err := m.runner.Run(ctx, "docker", args...); err != nil {
		return Credentials{}, dockerStartError(m.port, out, err)
	}

	if err := m.waitReachable(ctx); err != nil {
		return Credentials{}, err
	}

	out, err := m.runner.Run(ctx, "docker", "exec", m.containerName,
		"forgejo", "admin", "user", "create",
		"--admin",
		"--username", in.Username,
		"--password", in.Password,
		"--email", in.Username+"@"+EmailDomain)
	if err != nil {
		return Credentials{}, fmt.Errorf("creating forgejo admin user: %w (output=%s)", err, string(out))
	}

	token, err := m.generateAdminToken(ctx, in.Username, in.Password)
	if err != nil {
		return Credentials{}, fmt.Errorf("generating admin API token: %w", err)
	}

	return Credentials{
		Username: in.Username,
		Password: in.Password,
		Token:    token,
	}, nil
}

// generateAdminToken creates an admin-scoped API token for the given user
// via Forgejo's REST API, authenticating with basic auth (username +
// password we just set). Returns the SHA1 token string Forgejo issues.
//
// Visible only at creation time — FORGE persists it; if it's ever lost,
// re-running `forge system start` against a fresh data dir is the recovery
// path.
func (m *Manager) generateAdminToken(ctx context.Context, user, pwd string) (string, error) {
	body := []byte(`{"name":"forge-cli","scopes":["all"]}`)
	endpoint := fmt.Sprintf("%s/api/v1/users/%s/tokens", m.URL(), user)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, pwd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling forgejo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("forgejo create token: %d %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if parsed.SHA1 == "" {
		return "", fmt.Errorf("forgejo returned empty token: %s", string(respBody))
	}
	return parsed.SHA1, nil
}

// Stop stops and removes the Forgejo container. It is a no-op if the container
// does not exist.
func (m *Manager) Stop(ctx context.Context) error {
	running, _ := m.IsRunning(ctx)
	if running {
		if _, err := m.runner.Run(ctx, "docker", "stop", m.containerName); err != nil {
			return fmt.Errorf("docker stop: %w", err)
		}
		if _, err := m.runner.Run(ctx, "docker", "rm", m.containerName); err != nil {
			return fmt.Errorf("docker rm: %w", err)
		}
	}
	return nil
}

func (m *Manager) waitReachable(ctx context.Context) error {
	deadline := time.Now().Add(m.readyTimeout)
	var lastReason string
	for {
		ok, reason := m.reachable(ctx, m.URL())
		if ok {
			return nil
		}
		lastReason = reason
		if time.Now().After(deadline) {
			return fmt.Errorf("forgejo did not become reachable within %s: %s", m.readyTimeout, lastReason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Reachable performs an HTTP GET against `<url>/api/v1/version` and reports
// whether the response indicates Forgejo is ready. A 200 status is considered
// reachable; anything else (including a transport error) is reported as a
// human-readable reason.
func Reachable(ctx context.Context, url string) (bool, string) {
	client := &http.Client{Timeout: 3 * time.Second}
	endpoint := strings.TrimRight(url, "/") + "/api/v1/version"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return true, resp.Status
	}
	return false, resp.Status
}

// passwordPattern matches Forgejo's `... password is 'XYZ' ...` admin output.
var passwordPattern = regexp.MustCompile(`password\s+(?:is\s+)?'([^']+)'`)

// ParseRandomPassword extracts the random password from Forgejo's admin user
// create output. Forgejo prints the password between single quotes following
// the words "password is".
func ParseRandomPassword(output string) (string, error) {
	m := passwordPattern.FindStringSubmatch(output)
	if len(m) < 2 {
		return "", fmt.Errorf("no quoted password found in output")
	}
	return m[1], nil
}

// ExecRunner is the production CommandRunner; it shells out via os/exec.
type ExecRunner struct{}

// Run executes the command and returns its combined stdout+stderr output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// dockerStartError wraps `docker run` failures with actionable context.
// It always surfaces docker's combined output (which the caller would
// otherwise have to chase via `docker logs`) and adds a tailored hint
// when the failure was a port-already-in-use, since that's by far the
// most common cause and has two distinct fixes (rebind or reuse).
func dockerStartError(port int, out []byte, runErr error) error {
	output := strings.TrimSpace(string(out))
	if isPortInUse(output) {
		return fmt.Errorf(`port %d is already in use, so Forgejo could not start.

Two ways forward:

  1. Pick a different port for the FORGE-managed Forgejo. Edit
     ~/.forge/config.yaml and add:
         forgejo:
           port: <some free port>

  2. Reuse an existing Forgejo (e.g. one that CAGE is already running).
     Edit ~/.forge/config.yaml and set:
         forgejo:
           url:   "http://localhost:<existing port>"
           token: "<personal access token from that Forgejo>"
     With forgejo.url set, "forge system start" becomes a no-op.

docker output:
%s`, port, output)
	}
	if output != "" {
		return fmt.Errorf("docker run: %w\n%s", runErr, output)
	}
	return fmt.Errorf("docker run: %w", runErr)
}

// isPortInUse heuristically detects docker's "port already allocated"
// failure mode across the few wordings docker has shipped over the years.
func isPortInUse(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "port is already allocated") ||
		strings.Contains(low, "address already in use") ||
		strings.Contains(low, "bind for") && strings.Contains(low, "failed")
}
