package cloudinit_test

import (
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/p5n-dev/forge/internal/cloudinit"
)

func TestRenderUserData_ContainsAllVersionsAndKey(t *testing.T) {
	pubKey := "ssh-ed25519 AAAA... forge-env-test"
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "myproj",
		AuthorizedKey:     pubKey,
		K3sVersion:        "v1.32.0+k3s1",
		RageVersion:       "v0.4.2",
		ClaudeCodeVersion: "latest",
		HelmVersion:       "v3.20.2",
		ForgejoRemoteURL:  "http://192.168.127.1:3000/forge/myproj.git",
	})
	require.NoError(t, err)

	s := string(got)
	assert.True(t, strings.HasPrefix(s, "#cloud-config\n"),
		"output must begin with #cloud-config marker, got: %q", s[:min(40, len(s))])
	assert.Contains(t, s, "hostname: forge-myproj")
	assert.Contains(t, s, pubKey)
	// Versions are passed as positional args to forge-bootstrap; the
	// substrings still need to land somewhere in the rendered yaml.
	assert.Contains(t, s, "v1.32.0+k3s1")
	assert.Contains(t, s, "v0.4.2")
	// claude-code installs via the official curl https://claude.ai/install.sh
	// pipeline. The forge.yaml-supplied version lands as the script's
	// TARGET arg (after a v-prefix strip).
	// helm installs via the official get-helm-3 script with our
	// pinned content sha256. DESIRED_VERSION pins the helm release
	// without us having to track helm version+binary hash directly.
	assert.Contains(t, s, "raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3")
	assert.Contains(t, s, `DESIRED_VERSION="$HELM_VERSION" bash "$HELM_INSTALLER"`)
	assert.Contains(t, s, "v3.20.2", "forge.yaml-supplied helm version must reach the bootstrap")

	assert.Contains(t, s, "https://claude.ai/install.sh")
	assert.Contains(t, s, `CLAUDE_TARGET="${CLAUDE_VERSION#v}"`,
		"v-prefixed pins must be normalised before install.sh's TARGET regex sees them")
	assert.Contains(t, s, `sudo -u forge -H mkdir -p /home/forge/.local/bin`,
		"~/.local/bin must exist before install so Debian's .profile picks it up on next login")
	assert.Contains(t, s, `sudo -u forge -H env "PATH=/home/forge/.local/bin:$PATH"`,
		"PATH must include ~/.local/bin during install or claude warns about it post-install")
	assert.Contains(t, s, "ln -sf /home/forge/.local/bin/claude /usr/local/bin/claude",
		"claude must end up on /usr/local/bin so rage's child-process PATH finds it")
	assert.Contains(t, s, "http://192.168.127.1:3000/forge/myproj.git")

	// Regression guard: the old npm/pnpm/Node.js install path is gone.
	// claude.ai/install.sh ships a self-contained native binary, so
	// none of the JS toolchain should leak back into the bootstrap.
	assert.NotContains(t, s, "apt-get install -y npm")
	assert.NotContains(t, s, "pnpm-linux-arm64")
	assert.NotContains(t, s, "@anthropic-ai/claude-code",
		"npm package coordinate must not appear — install.sh owns the install path now")
	assert.NotContains(t, s, "nodejs.org/dist",
		"Node.js tarball fetch is gone; claude binary has no Node dependency")
	// forge-ready is what unblocks `forge env create`'s WaitReady.
	assert.Contains(t, s, "/usr/local/bin/forge-ready")
	// SSH-via-vsock systemd unit is what makes host→VM SSH work without
	// touching the routing table (so a corp VPN can't intercept it).
	assert.Contains(t, s, "/etc/systemd/system/forge-ssh-vsock.service")
	assert.Contains(t, s, "VSOCK-LISTEN:22,fork,reuseaddr TCP:127.0.0.1:22")
	assert.Contains(t, s, "systemctl enable --now forge-ssh-vsock.service")

	// Rage is installed by copying from the host-shared virtio-fs
	// mount (tag `rage-share`), NOT by fetching from a network URL.
	// Guard against accidental regression of the placeholder fetch.
	assert.NotContains(t, s, "releases.rage.internal",
		"rage must not be fetched from the network — it's a virtio-fs copy from ~/.forge/rage")
	assert.Contains(t, s, "mount -t virtiofs -o ro rage-share /mnt/rage-host")
	assert.Contains(t, s, "rage-${ARCH}-linux",
		"binary must be picked by guest's `uname -m` so x86_64 hosts work too once supported")
	assert.Contains(t, s, "install -m 0755")
	assert.Contains(t, s, "/home/forge/.config/rage/rage.toml")

	// virtio-fs workspace mount: cloud-init renders an fstab entry via
	// the `mounts:` directive so a reboot doesn't lose the mount.
	assert.Contains(t, s, "workspace-share, /home/forge/workspace, virtiofs",
		"workspace-share must be mounted at /home/forge/workspace in fstab")
	assert.Contains(t, s, "/etc/modules-load.d/forge-virtiofs.conf",
		"virtiofs module must be loaded at boot for the fstab mount to succeed")

	// Ghostty terminfo fallback: appended to forge's .bashrc so any
	// interactive command (git config --get-regexp, less, …) does not
	// stall on the "WARNING: terminal is not fully functional" prompt.
	assert.Contains(t, s, `xterm-ghostty) export TERM=xterm-256color`)

	// require_cmd MUST gate the bootstrap: a missing tool from the
	// base image (e.g. jq) silently broke the claude statusline once
	// — fail fast and clearly instead. Keep this list in lock-step
	// with images/base/build.sh.
	assert.Contains(t, s, "require_cmd curl jq git socat sudo install tar base64 sha256sum systemctl python3",
		"forge-bootstrap must verify base-image deps before doing anything else")
	assert.Contains(t, s, "ERROR: required commands missing from base image",
		"missing-deps error message must point operators at the base image")

	// Swap file is created BEFORE the heavy installs (k3s, claude
	// native binary) so the OOM killer doesn't take out the install
	// step on default 4 GB envs. Regression guard: a missing swapon
	// returns the "Killed" failure mode the user hit.
	assert.Contains(t, s, "mkswap /var/swap.img")
	assert.Contains(t, s, "swapon /var/swap.img")
	assert.Contains(t, s, "/var/swap.img none swap sw 0 0",
		"fstab entry must persist swap across reboots")

	// Claude Code statusline must land in /home/forge/.claude/ as
	// forge-owned files, with settings.json pointing at the script.
	assert.Contains(t, s, "/home/forge/.claude/statusline.sh",
		"embedded statusline.sh must be written under the forge user's .claude dir")
	assert.Contains(t, s, "/home/forge/.claude/settings.json",
		"settings.json must point claude at the statusline script")
	assert.Contains(t, s, "install -d -o forge -g forge -m 0755 /home/forge/.claude",
		"/home/forge/.claude must be pre-created as forge for the same install-d-on-deep-path reason as .config")
	assert.Contains(t, s, `"command": "~/.claude/statusline.sh"`)

	// /home/forge/.config must be forge-owned BEFORE any install step
	// that uses 'install -d <subdir>' on a deep path. Otherwise the
	// parent ends up root-owned (install -d only chowns the leaf),
	// and tools like 'helm repo add' fail with "mkdir
	// /home/forge/.config/helm: permission denied".
	assert.Contains(t, s, "install -d -o forge -g forge -m 0755 /home/forge/.config")

	// /home/forge ownership: cloud-init on Debian 13 cloud-arm64 has
	// been observed to leave the home dir owned by root:root despite
	// the users: uid spec. Without this fix, git push succeeds but
	// the credential helper fails with "Permission denied" trying to
	// create .git-credentials.lock in the parent dir.
	assert.Contains(t, s, "chown -R forge:forge /home/forge")
}

func TestRenderUserData_ForgeUserUIDMatchesHostWhenSet(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "demo",
		AuthorizedKey:     "ssh-ed25519 AAAA test",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
		HostUID:           501,
	})
	require.NoError(t, err)
	assert.Contains(t, string(got), "uid: 501",
		"forge user UID must match the host so workspace virtio-fs file ownership lines up")
}

func TestRenderUserData_OmitsUIDWhenZero(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "demo",
		AuthorizedKey:     "ssh-ed25519 AAAA test",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
		// HostUID intentionally zero
	})
	require.NoError(t, err)
	assert.NotContains(t, string(got), "uid:",
		"empty HostUID must NOT render `uid:` (cloud-init would interpret 0 as root)")
}

func TestRenderUserData_GitCredentialsAndForgejoVsock(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "demo",
		AuthorizedKey:     "ssh-ed25519 AAAA test",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
		ForgejoHostBase:   "http://localhost:4000",
		ForgejoVsockPort:  4000,
		ForgejoUser:       "forge",
		ForgejoToken:      "secrettoken",
		GitUserName:       "demo",
		GitUserEmail:      "demo@forge.local",
	})
	require.NoError(t, err)
	s := string(got)

	// Credentials file uses the same URL string the host clone records
	// in .git/config — the in-VM socat unit forwards localhost:<port>
	// over vsock so no host/VM URL split is needed.
	assert.Contains(t, s, "/home/forge/.git-credentials")
	assert.Contains(t, s, "http://forge:secrettoken@localhost:4000")

	// /home/forge/.gitconfig pre-seeds identity + credential helper.
	// We deliberately do NOT emit an `[url ".../"] insteadOf` block:
	// with the vsock forwarder the URL works as-is in both host and
	// VM views, so a rewrite would be a self-rewrite no-op.
	assert.Contains(t, s, "/home/forge/.gitconfig")
	assert.Contains(t, s, "name = demo")
	assert.Contains(t, s, "email = demo@forge.local")
	assert.Contains(t, s, "helper = store")
	assert.NotContains(t, s, "insteadOf",
		"insteadOf is obsolete now that host/VM URLs match — leaving it would cause confusion")

	// In-VM socat unit + runcmd enable: the actual VPN-immune wiring.
	assert.Contains(t, s, "/etc/systemd/system/forge-forgejo-vsock.service")
	assert.Contains(t, s, "TCP-LISTEN:4000,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:4000")
	assert.Contains(t, s, "systemctl enable --now forge-forgejo-vsock.service")

	// Regression guard against the old runcmd-based git config approach
	// that left .gitconfig unwritable. Match the YAML list-entry shape
	// so the explanatory comment in the template doesn't trip it.
	assert.NotContains(t, s, "- sudo -u forge",
		".gitconfig is now written declaratively; runcmd-based `git config --global` caused permission-denied bugs")
}

func TestRenderUserData_OmitsGitCredentialsWhenForgejoUnset(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "demo",
		AuthorizedKey:     "ssh-ed25519 AAAA test",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
		// All Forgejo fields empty.
	})
	require.NoError(t, err)
	s := string(got)
	assert.NotContains(t, s, "/home/forge/.git-credentials",
		"no credential file should be written when Forgejo is not configured")
	assert.NotContains(t, s, "forge-forgejo-vsock.service",
		"no in-VM socat unit when there's no Forgejo to bridge to")
	assert.NotContains(t, s, "credential.helper",
		"no git config runcmd should fire when Forgejo is not configured")
}

func TestRenderUserData_IsValidYAML(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "x",
		AuthorizedKey:     "ssh-ed25519 AAAA... x",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
	})
	require.NoError(t, err)

	// Skip the "#cloud-config" header line for YAML parsing.
	body := strings.SplitN(string(got), "\n", 2)[1]
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(body), &doc))
	assert.Contains(t, doc, "users")
	assert.Contains(t, doc, "runcmd")
	assert.Contains(t, doc, "hostname")
}

func TestRenderUserData_RequiresName(t *testing.T) {
	_, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		AuthorizedKey:     "k",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name")
}

func TestRenderUserData_RequiresAuthorizedKey(t *testing.T) {
	_, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "x",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorized key")
}

// TestStatusline_FallbackPaths runs the embedded script directly with
// stub JSON shapes that mimic what claude code 2.1.132+ may pipe in,
// to ensure the script doesn't silently fall back to "claude" /
// empty folder when the JSON's structure shifts.
func TestStatusline_FallbackPaths(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH (statusline depends on jq for JSON parsing)")
	}

	scriptPath, err := filepath.Abs("files/statusline.sh")
	require.NoError(t, err)

	cases := []struct {
		name     string
		input    string
		contains []string // must appear in rendered output
	}{
		{
			name:  "model is bare string",
			input: `{"model":"claude-opus-4-7","cwd":"/home/forge/workspace"}`,
			contains: []string{
				"opus",      // case-statement matched the substring
				"workspace", // basename of cwd
			},
		},
		{
			name:  "only display_name set on model object",
			input: `{"model":{"display_name":"Sonnet 4.6"},"cwd":"/home/forge/workspace"}`,
			contains: []string{
				"sonnet",
				"workspace",
			},
		},
		{
			name:  "fully empty JSON falls back to PWD and verbatim claude",
			input: `{}`,
			contains: []string{
				"claude", // empty model path
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", scriptPath)
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "stderr: %s", string(out))
			rendered := string(out)
			for _, want := range tc.contains {
				assert.Contains(t, rendered, want, "rendered output missing %q", want)
			}
		})
	}
}

// TestRenderUserData_StatuslineRoundTrips verifies the embedded
// statusline.sh survives base64 encoding into the cidata YAML and
// would land on disk byte-identical after cloud-init decodes it.
// Catches regressions where the embed pattern, the encoding, or the
// template indentation drifts.
func TestRenderUserData_StatuslineRoundTrips(t *testing.T) {
	got, err := cloudinit.RenderUserData(cloudinit.UserDataInput{
		Name:              "demo",
		AuthorizedKey:     "ssh-ed25519 AAAA test",
		K3sVersion:        "v1",
		RageVersion:       "v1",
		ClaudeCodeVersion: "v1",
		HelmVersion:       "v3",
	})
	require.NoError(t, err)

	// Pull the b64 blob out of the rendered YAML. It now lives in a
	// bash heredoc inside forge-bootstrap (cloud-init's defer:true
	// write_files wedged cloud-final on Debian trixie's cloud-init
	// 25.x — see the comment in userdata.go's bootstrap section).
	re := regexp.MustCompile(`(?s)<<'STATUSLINE_B64'\s*([A-Za-z0-9+/=\s]+?)\s*STATUSLINE_B64`)
	m := re.FindSubmatch(got)
	require.NotNil(t, m, "could not locate base64 statusline blob in rendered bootstrap heredoc")

	decoded, err := base64.StdEncoding.DecodeString(string(m[1]))
	require.NoError(t, err)

	// Two checks: it's the bash script (shebang + the catppuccin
	// header comment), and it's the FULL script (printf line at the
	// bottom, post-truncation regression guard).
	assert.True(t, strings.HasPrefix(string(decoded), "#!/usr/bin/env bash\n"),
		"decoded statusline must start with the bash shebang")
	assert.Contains(t, string(decoded), "Catppuccin Mocha statusline")
	assert.Contains(t, string(decoded), "printf '%b'",
		"trailing printf line must be present — regression guard for truncation/indentation drift")
}

func TestRenderMetaData(t *testing.T) {
	got := cloudinit.RenderMetaData("myproj")
	s := string(got)
	assert.Contains(t, s, "instance-id: forge-myproj")
	assert.Contains(t, s, "local-hostname: forge-myproj")
}

func TestRenderNetworkConfig_StaticIP(t *testing.T) {
	// Regression guard: the cidata network-config must put a usable
	// static IP, default route, and DNS on enp0s1. We don't use DHCP
	// because vfkit's vmnet shared mode relies on macOS bootpd, which
	// is unreliable when other vmnet consumers are on the host.
	got, err := cloudinit.RenderNetworkConfig(cloudinit.NetworkConfigInput{
		Address: "192.168.127.42",
		Prefix:  24,
		Gateway: "192.168.127.1",
		DNS:     []string{"192.168.127.1", "1.1.1.1"},
	})
	require.NoError(t, err)
	s := string(got)
	assert.Contains(t, s, "version: 2")
	assert.Contains(t, s, "enp0s1:")
	assert.Contains(t, s, "addresses: [192.168.127.42/24]")
	assert.Contains(t, s, "via: 192.168.127.1")
	assert.Contains(t, s, "192.168.127.1, 1.1.1.1")
	assert.NotContains(t, s, "dhcp4")
}

func TestRenderNetworkConfig_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		in   cloudinit.NetworkConfigInput
	}{
		{"no address", cloudinit.NetworkConfigInput{Prefix: 24, Gateway: "g", DNS: []string{"d"}}},
		{"no prefix", cloudinit.NetworkConfigInput{Address: "a", Gateway: "g", DNS: []string{"d"}}},
		{"no gateway", cloudinit.NetworkConfigInput{Address: "a", Prefix: 24, DNS: []string{"d"}}},
		{"no dns", cloudinit.NetworkConfigInput{Address: "a", Prefix: 24, Gateway: "g"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cloudinit.RenderNetworkConfig(tc.in)
			require.Error(t, err)
		})
	}
}
