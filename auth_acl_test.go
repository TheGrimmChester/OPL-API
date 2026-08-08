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

	// Multi X-Project-IDs: any id outside the JWT allowlist → 403.
	multiDeny := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	multiDeny.Header.Set("Authorization", "Bearer "+tok)
	multiDeny.Header.Set("X-Organization-ID", "default-org")
	multiDeny.Header.Set("X-Project-IDs", "allowed-only,other-project")
	rec4 := httptest.NewRecorder()
	h(rec4, multiDeny)
	if rec4.Code != http.StatusForbidden {
		t.Fatalf("multi IDs over-ACL: got %d want 403 body=%s", rec4.Code, rec4.Body.String())
	}

	multiOK := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	multiOK.Header.Set("Authorization", "Bearer "+tok)
	multiOK.Header.Set("X-Organization-ID", "default-org")
	multiOK.Header.Set("X-Project-IDs", "allowed-only")
	rec5 := httptest.NewRecorder()
	h(rec5, multiOK)
	if rec5.Code != http.StatusOK {
		t.Fatalf("multi IDs in-ACL: got %d want 200 body=%s", rec5.Code, rec5.Body.String())
	}
}

func TestAuthMiddlewarePersonalRejectsOrgHeader(t *testing.T) {
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

	tok, err := openauth.MintUserJWTWithAccount(
		authGate.Secret, "alice", "viewer", "opl-api",
		openauth.AccountTypePersonal, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "viewer")
	req := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Organization-ID", "acme")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("personal + org header: got %d want 403 body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddlewareNoJWT(t *testing.T) {
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

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "viewer")
	req := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("no JWT: got %d want 401 or 403", rec.Code)
	}
}
