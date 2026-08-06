package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openauth "github.com/TheGrimmChester/open-auth-go"
)

const (
	testOPLUserSecret    = "test-user-jwt-secret-at-least-32-bytes!"
	testOPLServiceSecret = "test-service-jwt-secret-distinct-32b!!"
)

func newOPLPeerAuthMux(t *testing.T) *http.ServeMux {
	t.Helper()
	t.Setenv("JWT_SECRET", testOPLUserSecret)
	t.Setenv("OPEN_SERVICE_JWT_SECRET", testOPLServiceSecret)
	t.Setenv("AUTH_MODE", "codeployed")
	t.Setenv("OPA_AUTH_REQUIRED", "1")

	prevGate := authGate
	prevAuth := authEnforced
	t.Cleanup(func() {
		authGate = prevGate
		authEnforced = prevAuth
	})

	initAuthMode()
	setAuthEnforced(true)
	if authGate == nil {
		t.Fatal("expected auth gate")
	}

	mux := http.NewServeMux()
	registerPeerSCMMux(mux)
	return mux
}

func mintOPLPeerToken(t *testing.T, scope string) string {
	t.Helper()
	tok, err := openauth.MintServiceJWTWithOrg(
		[]byte(testOPLServiceSecret), "ora-api", "opl-api", scope, "org-test", 0)
	if err != nil {
		t.Fatalf("mint service jwt: %v", err)
	}
	return tok
}

func TestPeerSCMEventsAcceptsServiceJWT(t *testing.T) {
	mux := newOPLPeerAuthMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/peer/scm/events", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+mintOPLPeerToken(t, peerSCMEventsScope))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("service JWT rejected: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"checkers"`) {
		t.Fatalf("expected checkers in body: %s", rr.Body.String())
	}
}

func TestPeerSCMEventsRejectsForeignAudience(t *testing.T) {
	mux := newOPLPeerAuthMux(t)

	tok, err := openauth.MintServiceJWTWithOrg(
		[]byte(testOPLServiceSecret), "ora-api", "osa-api", peerSCMEventsScope, "org-test", 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/peer/scm/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-audience JWT accepted: code=%d body=%s", rr.Code, rr.Body.String())
	}
}
