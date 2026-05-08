package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VerifyAdminLogin authenticates as <login> via basic auth and confirms
// the credentials carry admin scope by hitting an admin-only endpoint
// (`/api/v1/admin/users`). It then resolves the canonical username via
// `/api/v1/user` and returns it — Forgejo accepts either username or
// email for basic-auth logins, but path-based endpoints
// (`/api/v1/users/<name>/...`) require the actual username, so we
// always normalise here before downstream calls.
//
// Used by `forge system start` when reusing an external Forgejo, before
// we commit anything to ~/.forge/config.yaml. Errors are tailored to
// common failure modes — bad creds (401), not-admin (403), unreachable
// host — so the user gets a clear hint rather than a raw HTTP status.
func VerifyAdminLogin(ctx context.Context, baseURL, login, pwd string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/admin/users?limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(login, pwd)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach Forgejo at %s: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return resolveLogin(ctx, client, baseURL, login, pwd)
	case http.StatusUnauthorized:
		return "", fmt.Errorf("credentials rejected for user %q (check username/password)", login)
	case http.StatusForbidden:
		return "", fmt.Errorf("user %q does not have admin scope on this Forgejo (FORGE needs admin to manage per-env users)", login)
	default:
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("forgejo returned %d %s when verifying admin login: %s",
			resp.StatusCode, resp.Status, string(body))
	}
}

// resolveLogin asks Forgejo who the authenticated user is, returning
// the canonical username (the `login` field of `/api/v1/user`). This
// is required because Forgejo treats username and email as
// interchangeable for basic auth, but path-style API endpoints only
// accept the username form.
func resolveLogin(ctx context.Context, client *http.Client, baseURL, login, pwd string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(login, pwd)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolving canonical username: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("resolving canonical username: %d %s", resp.StatusCode, string(body))
	}

	var who struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &who); err != nil {
		return "", fmt.Errorf("parsing /api/v1/user response: %w", err)
	}
	if who.Login == "" {
		return "", fmt.Errorf("forgejo returned empty login field for authenticated user")
	}
	return who.Login, nil
}

type tokenListEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tokenCreateBody struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type tokenCreateResponse struct {
	SHA1 string `json:"sha1"`
}

// EnsureCLIToken provisions a fresh API token for the given user. If a
// token of the same name already exists, it is deleted first — Forgejo
// only exposes the token value at creation time, so "reuse" is not
// possible; the practical equivalent is "always own a working one".
//
// The token has the "all" scope, which inherits whatever permissions the
// authenticating user has — admin in this case, since the caller is
// expected to have just verified that.
func EnsureCLIToken(ctx context.Context, baseURL, user, pwd, tokenName string) (string, error) {
	if err := deleteExistingToken(ctx, baseURL, user, pwd, tokenName); err != nil {
		return "", err
	}
	return createToken(ctx, baseURL, user, pwd, tokenName)
}

func deleteExistingToken(ctx context.Context, baseURL, user, pwd, tokenName string) error {
	listURL := fmt.Sprintf("%s/api/v1/users/%s/tokens",
		strings.TrimRight(baseURL, "/"), url.PathEscape(user))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pwd)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("listing tokens: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("credentials rejected for user %q", user)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("listing tokens: %d %s", resp.StatusCode, string(body))
	}

	var list []tokenListEntry
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("parsing token list: %w", err)
	}

	for _, tok := range list {
		if tok.Name != tokenName {
			continue
		}
		// Forgejo's DELETE endpoint accepts either the numeric ID or the
		// token name in the path; the name form is more stable across
		// API versions and easier to read in logs.
		delURL := fmt.Sprintf("%s/api/v1/users/%s/tokens/%s",
			strings.TrimRight(baseURL, "/"),
			url.PathEscape(user), url.PathEscape(tokenName))

		dreq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
		if err != nil {
			return err
		}
		dreq.SetBasicAuth(user, pwd)
		dresp, err := client.Do(dreq)
		if err != nil {
			return fmt.Errorf("deleting old token: %w", err)
		}
		_ = dresp.Body.Close()
		if dresp.StatusCode < 200 || dresp.StatusCode >= 300 {
			return fmt.Errorf("deleting old token: %d %s", dresp.StatusCode, dresp.Status)
		}
		// There can only be one token per name in Forgejo (creation is
		// rejected with 422 otherwise), so we can stop on first match.
		return nil
	}
	return nil
}

func createToken(ctx context.Context, baseURL, user, pwd, tokenName string) (string, error) {
	body, err := json.Marshal(tokenCreateBody{Name: tokenName, Scopes: []string{"all"}})
	if err != nil {
		return "", err
	}

	endpoint := fmt.Sprintf("%s/api/v1/users/%s/tokens",
		strings.TrimRight(baseURL, "/"), url.PathEscape(user))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, pwd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("creating token: %d %s", resp.StatusCode, string(respBody))
	}

	var parsed tokenCreateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if parsed.SHA1 == "" {
		return "", fmt.Errorf("forgejo returned empty token: %s", string(respBody))
	}
	return parsed.SHA1, nil
}
