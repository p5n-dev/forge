package forgejo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/p5n-dev/forge/internal/forgejo"
)

func TestVerifyAdminLogin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPwd, ok := r.BasicAuth()
		require.True(t, ok, "basic auth must be sent")
		assert.Equal(t, "admin", gotUser)
		assert.Equal(t, "secret", gotPwd)
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/admin/users"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/user"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"login":"admin"}`))
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	got, err := forgejo.VerifyAdminLogin(context.Background(), srv.URL, "admin", "secret")
	require.NoError(t, err)
	assert.Equal(t, "admin", got)
}

func TestVerifyAdminLogin_EmailLoginResolvesToUsername(t *testing.T) {
	// CAGE creates admin users with email "admin@cage.local" and username
	// "admin" — Forgejo accepts either for basic auth, but the path-style
	// token endpoints require the username. VerifyAdminLogin must
	// normalise.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/admin/users"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/user"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"login":"admin"}`))
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	got, err := forgejo.VerifyAdminLogin(context.Background(), srv.URL, "admin@cage.local", "secret")
	require.NoError(t, err)
	assert.Equal(t, "admin", got, "must return canonical username, not the email used to log in")
}

func TestVerifyAdminLogin_BadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := forgejo.VerifyAdminLogin(context.Background(), srv.URL, "admin", "wrong")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentials", "401 should say creds are bad")
}

func TestVerifyAdminLogin_NotAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := forgejo.VerifyAdminLogin(context.Background(), srv.URL, "alice", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin", "403 should say the user lacks admin scope")
}

func TestVerifyAdminLogin_Unreachable(t *testing.T) {
	_, err := forgejo.VerifyAdminLogin(context.Background(), "http://127.0.0.1:1", "u", "p")
	require.Error(t, err)
}

func TestEnsureCLIToken_CreatesFreshWhenNoneExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v1/users/admin/tokens"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/users/admin/tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sha1":"new-tok"}`))
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	tok, err := forgejo.EnsureCLIToken(context.Background(), srv.URL, "admin", "pwd", "forge-cli")
	require.NoError(t, err)
	assert.Equal(t, "new-tok", tok)
}

func TestEnsureCLIToken_DeletesExistingThenCreates(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v1/users/admin/tokens"):
			w.WriteHeader(http.StatusOK)
			// Existing token named "forge-cli" — must be deleted before recreation.
			_, _ = w.Write([]byte(`[{"id":42,"name":"forge-cli"}]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v1/users/admin/tokens/forge-cli"):
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/users/admin/tokens"):
			if !deleted {
				t.Errorf("POST happened before DELETE")
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sha1":"replacement-tok"}`))
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	tok, err := forgejo.EnsureCLIToken(context.Background(), srv.URL, "admin", "pwd", "forge-cli")
	require.NoError(t, err)
	assert.Equal(t, "replacement-tok", tok)
	assert.True(t, deleted, "old token must be deleted before fresh one is created")
}

func TestEnsureCLIToken_BadCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := forgejo.EnsureCLIToken(context.Background(), srv.URL, "admin", "wrong", "forge-cli")
	require.Error(t, err)
}
