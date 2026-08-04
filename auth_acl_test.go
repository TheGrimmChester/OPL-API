package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	openauth "github.com/TheGrimmChester/open-auth-go"
)

// AuthMiddleware → Gate.RequireUser already calls EnforceProjectACL after
// ApplyUserTenantHeaders (Open-Auth-Go #6). These tests pin product wiring.
func TestAuthMiddlewareProjectACL(t *testing.T) {
	prevGate := authGate
	t.Cleanup(func() { authGate = prevGate })

	secret := "test-jwt-secret-at-least-32-bytes-ok!!"
	t.Setenv("JWT_SECRET", secret)
	t.Setenv("OPA_AUTH_REQUIRED", "1")
	t.Setenv("AUTH_MODE", "codeployed")
	t.Setenv("PEER_OPA_URL", "http://127.0.0.1:18080")
	initAuthMode()
	if authGate == nil {
		t.Fatal("expected auth gate")
	}

	tok, err := openauth.MintUserJWTWithACL(
		authGate.Secret, "dev", "viewer", "opl-api",
		"default-org", []string{"allowed-only"}, 0,
	)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "viewer")

	deny := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	deny.Header.Set("Authorization", "Bearer "+tok)
	deny.Header.Set("X-Organization-ID", "default-org")
	deny.Header.Set("X-Project-ID", "other-project")
	rec := httptest.NewRecorder()
	h(rec, deny)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ACL deny: got %d want 403 body=%s", rec.Code, rec.Body.String())
	}

	allow := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	allow.Header.Set("Authorization", "Bearer "+tok)
	allow.Header.Set("X-Organization-ID", "default-org")
	allow.Header.Set("X-Project-ID", "allowed-only")
	rec2 := httptest.NewRecorder()
	h(rec2, allow)
	if rec2.Code != http.StatusOK {
		t.Fatalf("ACL allow: got %d want 200 body=%s", rec2.Code, rec2.Body.String())
	}

	adminTok, err := openauth.MintUserJWT(authGate.Secret, "admin", "admin", "opl-api", 0)
	if err != nil {
		t.Fatal(err)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	adminReq.Header.Set("Authorization", "Bearer "+adminTok)
	adminReq.Header.Set("X-Organization-ID", "default-org")
	adminReq.Header.Set("X-Project-ID", "any-project")
	rec3 := httptest.NewRecorder()
	h(rec3, adminReq)
	if rec3.Code != http.StatusOK {
		t.Fatalf("admin unrestricted: got %d want 200", rec3.Code)
	}
}
