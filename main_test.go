package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/miista/authy"
	"github.com/miista/packrat/internal/scheduler"
	"github.com/miista/packrat/internal/store"
)

// handlerFor builds the real handler with a gate configured from env, the way
// main does, over a throwaway data dir and dashboard.
func handlerFor(t *testing.T, env map[string]string) http.Handler {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	static := filepath.Join(dir, "static")
	if err := os.MkdirAll(static, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("<html>dashboard</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gate, err := authy.FromEnv(ctx, "packrat", authy.WithDataDir(filepath.Join(dir, "auth")))
	if err != nil {
		t.Fatal(err)
	}
	log := zerolog.Nop()
	return newHandler(st, scheduler.New(st, log, nil, nil), gate, static, log)
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// An OIDC provider that nothing answers is fine for these tests: the gate
// retries in the background and refuses everything meanwhile.
var oidcEnv = map[string]string{
	"PACKRAT_OIDC_ISSUER":        "http://127.0.0.1:1",
	"PACKRAT_OIDC_CLIENT_ID":     "packrat",
	"PACKRAT_OIDC_CLIENT_SECRET": "secret",
	"PACKRAT_OIDC_BASE_URL":      "https://packrat.example.test",
}

func TestGateRefusesUnauthenticatedRequests(t *testing.T) {
	h := handlerFor(t, oidcEnv)

	// Every API route, read and write: none may answer without a session.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/state"},
		{"GET", "/api/history"},
		{"GET", "/api/settings"},
		{"POST", "/api/settings"},
		{"POST", "/api/start"},
		{"POST", "/api/pause"},
		{"POST", "/api/run"},
		{"POST", "/api/dry-run"},
	} {
		rec := do(h, tc.method, tc.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", tc.method, tc.path, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unauthorized"}` {
			t.Errorf("%s %s body = %q, want the gate's JSON 401", tc.method, tc.path, got)
		}
	}

	// The dashboard itself is behind the gate too, not just the API.
	rec := do(h, "GET", "/", map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"})
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Errorf("page load without a session = %d, want a redirect", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "dashboard") {
		t.Error("page load without a session served the dashboard")
	}
}

func TestGateClosesLocalSetupWhenOIDCIsConfigured(t *testing.T) {
	h := handlerFor(t, oidcEnv)
	for _, path := range []string{"/auth/setup", "/auth/local"} {
		rec := do(h, "GET", path, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 with OIDC configured and local login off; nobody may claim the app by being first", path)
		}
	}
}

func TestHealthzIsPublic(t *testing.T) {
	h := handlerFor(t, oidcEnv)
	rec := do(h, "GET", "/healthz", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("GET /healthz = %d %q, want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
}

func TestAuthDisabledServesWithoutSignIn(t *testing.T) {
	h := handlerFor(t, map[string]string{"PACKRAT_AUTH_DISABLED": "true"})
	rec := do(h, "GET", "/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/state with the gate off = %d, want 200", rec.Code)
	}
	if rec := do(h, "GET", "/auth/client.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /auth/client.js with the gate off = %d, want 404 (keeps Sign out hidden)", rec.Code)
	}
}

func TestIncompleteOIDCConfigurationIsRefused(t *testing.T) {
	t.Setenv("PACKRAT_OIDC_ISSUER", "https://auth.example.test")
	// The other three are unset: starting anyway would fall back to the local
	// account, so FromEnv has to refuse.
	if _, err := authy.FromEnv(context.Background(), "packrat", authy.WithDataDir(t.TempDir())); err == nil {
		t.Error("FromEnv accepted a partial OIDC configuration")
	}
}
