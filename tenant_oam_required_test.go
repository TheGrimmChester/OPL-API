package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTenantMiddlewareAuthRequiresOAM(t *testing.T) {
	prev := authEnforced
	setAuthEnforced(true)
	t.Cleanup(func() {
		setAuthEnforced(prev)
		t.Setenv("PEER_OAM_URL", "")
	})
	t.Setenv("PEER_OAM_URL", "")

	called := false
	h := TenantMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("X-Organization-ID", "acme")
	req.Header.Set("X-Project-ID", "proj")
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("auth without OAM: got %d want 503 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PEER_OAM_URL") {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if called {
		t.Fatal("handler must not run")
	}
}

func TestTenantMiddlewareSkipsCHWhenOAMSet(t *testing.T) {
	prev := authEnforced
	setAuthEnforced(true)
	t.Cleanup(func() {
		setAuthEnforced(prev)
		t.Setenv("PEER_OAM_URL", "")
	})
	t.Setenv("PEER_OAM_URL", "http://oam:8090")

	called := false
	var sawOrg string
	h := TenantMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		sawOrg = r.Header.Get("X-Organization-ID")
		w.WriteHeader(http.StatusOK)
	}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("X-Organization-ID", "acme")
	req.Header.Set("X-Project-ID", "proj")
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OAM set: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if !called || sawOrg != "acme" {
		t.Fatalf("handler org=%q called=%v", sawOrg, called)
	}
}
