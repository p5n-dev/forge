package forgejo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EmailDomain is the suffix FORGE uses for the synthetic email it gives
// each per-env Forgejo user. The address is never delivered to — Forgejo
// just requires a non-empty value, and we want it to be obvious where
// these accounts came from.
const EmailDomain = "forge.local"

// WorkspaceRepoName is the single repo every per-env Forgejo user owns.
// Mirrors the convention CAGE established (envs land at <env>/workspace).
const WorkspaceRepoName = "workspace"

// APIClient is a small wrapper for the subset of the Forgejo REST API that
// FORGE uses. It is independent of the Docker-based Manager so the same code
// works against an externally hosted Forgejo. The configured token must
// have admin scope so the per-env user-creation flow works.
type APIClient struct {
	baseURL string
	user    string
	token   string
	http    *http.Client
}

// NewAPIClient constructs an APIClient. baseURL must include scheme and host
// (e.g. "http://localhost:3000"); trailing slashes are tolerated.
func NewAPIClient(baseURL, user, token string) *APIClient {
	return &APIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// EnsureRepo provisions the FORGE-style "<envName>/workspace" repository:
//
//  1. Ensure a Forgejo user named envName exists (admin endpoint, idempotent).
//  2. Ensure a repo named "workspace" exists in that user's namespace.
//
// Both steps tolerate the "already exists" cases so re-running is safe.
// Returns the clone URL the agent will use as `git remote add origin`.
func (c *APIClient) EnsureRepo(ctx context.Context, envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("forgejo: env name is required")
	}
	if err := c.ensureUser(ctx, envName); err != nil {
		return "", err
	}
	return c.ensureWorkspaceRepo(ctx, envName)
}

// PurgeEnvUser tears down a per-env Forgejo identity along with every repo
// it owns. Used by `forge env destroy --purge-forgejo`. A 404 is treated as
// success — the goal is "user is gone", and they may already be gone if a
// previous destroy ran without --purge-forgejo and was later cleaned up by
// hand.
func (c *APIClient) PurgeEnvUser(ctx context.Context, envName string) error {
	if envName == "" {
		return fmt.Errorf("forgejo: env name is required")
	}

	endpoint := fmt.Sprintf("%s/api/v1/admin/users/%s?purge=true",
		c.baseURL, url.PathEscape(envName))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	c.authHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling forgejo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("forgejo purge user: %d %s", resp.StatusCode, string(body))
	}
	return nil
}

// --- private ---

type adminUserCreateBody struct {
	Username           string `json:"username"`
	Email              string `json:"email"`
	Password           string `json:"password"`
	MustChangePassword bool   `json:"must_change_password"`
	SourceID           int    `json:"source_id"`
}

func (c *APIClient) ensureUser(ctx context.Context, username string) error {
	pwd, err := randomPassword()
	if err != nil {
		return fmt.Errorf("generating user password: %w", err)
	}
	body := adminUserCreateBody{
		Username:           username,
		Email:              username + "@" + EmailDomain,
		Password:           pwd,
		MustChangePassword: false,
		SourceID:           0,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshalling admin user body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/admin/users", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	c.authHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling forgejo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusUnprocessableEntity, http.StatusConflict:
		// Forgejo signals "user already exists" with 422; treat as idempotent.
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("forgejo create user: %d %s", resp.StatusCode, string(respBody))
}

type repoCreateBody struct {
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	AutoInit      bool   `json:"auto_init"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

type repoResponse struct {
	CloneURL string `json:"clone_url"`
}

func (c *APIClient) ensureWorkspaceRepo(ctx context.Context, owner string) (string, error) {
	body := repoCreateBody{
		Name:          WorkspaceRepoName,
		Private:       true,
		AutoInit:      true,
		DefaultBranch: "main",
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshalling repo body: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/admin/users/%s/repos",
		c.baseURL, url.PathEscape(owner))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	c.authHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling forgejo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict ||
		resp.StatusCode == http.StatusUnprocessableEntity {
		return c.lookupCloneURL(ctx, owner)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("forgejo create repo: %d %s", resp.StatusCode, string(respBody))
	}

	var r repoResponse
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("parsing forgejo response: %w", err)
	}
	return r.CloneURL, nil
}

func (c *APIClient) lookupCloneURL(ctx context.Context, owner string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s",
		c.baseURL, url.PathEscape(owner), WorkspaceRepoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	c.authHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling forgejo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("forgejo get repo: %d %s", resp.StatusCode, string(body))
	}

	var r repoResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("parsing forgejo response: %w", err)
	}
	return r.CloneURL, nil
}

func (c *APIClient) authHeaders(req *http.Request) {
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
}

// randomPassword returns 32 bytes of base64 — enough to satisfy any
// reasonable password policy. The password is never stored or surfaced;
// per-env users authenticate to FORGE's tooling via the admin token, not
// password. We just need Forgejo to accept the user-create call.
func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
