package forgejo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/forgejo"
)

// recordedCall captures a single inbound HTTP call so tests can assert on
// the sequence FORGE makes against Forgejo's admin API.
type recordedCall struct {
	method string
	path   string
	body   map[string]any
}

type recorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, _ := io.ReadAll(req.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	r.calls = append(r.calls, recordedCall{
		method: req.Method, path: req.URL.Path, body: parsed,
	})
}

func (r *recorder) snapshot() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestEnsureRepo_CreatesUserAndWorkspaceRepo(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"login":"myproj"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users/myproj/repos"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"clone_url":"http://forgejo/myproj/workspace.git"}`))
		default:
			t.Logf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	url, err := client.EnsureRepo(context.Background(), "myproj")
	require.NoError(t, err)
	assert.Equal(t, "http://forgejo/myproj/workspace.git", url)

	calls := rec.snapshot()
	require.Len(t, calls, 2, "expected user-create + repo-create")

	// Step 1: admin user create with the env name and forge.local email.
	assert.Equal(t, http.MethodPost, calls[0].method)
	assert.True(t, strings.HasSuffix(calls[0].path, "/api/v1/admin/users"))
	assert.Equal(t, "myproj", calls[0].body["username"])
	assert.Equal(t, "myproj@forge.local", calls[0].body["email"])
	assert.NotEmpty(t, calls[0].body["password"], "password is required by Forgejo")

	// Step 2: repo create on behalf of that user, named "workspace".
	assert.Equal(t, http.MethodPost, calls[1].method)
	assert.True(t, strings.HasSuffix(calls[1].path, "/api/v1/admin/users/myproj/repos"))
	assert.Equal(t, "workspace", calls[1].body["name"])
}

func TestEnsureRepo_UserAlreadyExistsIsIdempotent(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users"):
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"user already exists"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users/myproj/repos"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"clone_url":"http://forgejo/myproj/workspace.git"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	url, err := client.EnsureRepo(context.Background(), "myproj")
	require.NoError(t, err)
	assert.Equal(t, "http://forgejo/myproj/workspace.git", url)
}

func TestEnsureRepo_RepoAlreadyExistsReturnsCloneURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/admin/users/myproj/repos"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"repo already exists"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v1/repos/myproj/workspace"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"clone_url":"http://forgejo/myproj/workspace.git"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	url, err := client.EnsureRepo(context.Background(), "myproj")
	require.NoError(t, err)
	assert.Equal(t, "http://forgejo/myproj/workspace.git", url)
}

func TestEnsureRepo_UnexpectedUserCreationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	_, err := client.EnsureRepo(context.Background(), "myproj")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestEnsureRepo_RequiresName(t *testing.T) {
	client := forgejo.NewAPIClient("http://localhost:3000", "admin", "tok")
	_, err := client.EnsureRepo(context.Background(), "")
	require.Error(t, err)
}

func TestPurgeEnvUser_DeletesUserWithPurge(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/api/v1/admin/users/myproj"),
			"unexpected path: %s", r.URL.Path)
		// Forgejo's `purge=true` deletes user + repos + issues in one call.
		assert.Equal(t, "true", r.URL.Query().Get("purge"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	require.NoError(t, client.PurgeEnvUser(context.Background(), "myproj"))

	assert.Len(t, rec.snapshot(), 1)
}

func TestPurgeEnvUser_NotFoundIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := forgejo.NewAPIClient(srv.URL, "admin", "admin-tok")
	require.NoError(t, client.PurgeEnvUser(context.Background(), "alreadygone"),
		"missing user is not an error — purge is the goal, not the means")
}

func TestPurgeEnvUser_RequiresName(t *testing.T) {
	client := forgejo.NewAPIClient("http://localhost:3000", "admin", "tok")
	require.Error(t, client.PurgeEnvUser(context.Background(), ""))
}
