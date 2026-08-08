package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrchestratorRequestAllowed(t *testing.T) {
	t.Setenv("OPL_ORCHESTRATOR_TOKEN", "")

	loop := httptest.NewRequest(http.MethodGet, "/api/orchestrator/state", nil)
	loop.RemoteAddr = "127.0.0.1:12345"
	if !orchestratorRequestAllowed(loop) {
		t.Fatal("loopback must be allowed")
	}

	remote := httptest.NewRequest(http.MethodGet, "/api/orchestrator/state", nil)
	remote.RemoteAddr = "10.0.0.5:9999"
	if orchestratorRequestAllowed(remote) {
		t.Fatal("remote without token must be denied")
	}

	t.Setenv("OPL_ORCHESTRATOR_TOKEN", "secret-orch")
	if orchestratorRequestAllowed(remote) {
		t.Fatal("remote still denied without presenting token")
	}
	remote.Header.Set("X-OPL-Orchestrator-Token", "secret-orch")
	if !orchestratorRequestAllowed(remote) {
		t.Fatal("token header must allow remote")
	}
	bearer := httptest.NewRequest(http.MethodGet, "/api/orchestrator/schedules", nil)
	bearer.RemoteAddr = "10.0.0.5:9999"
	bearer.Header.Set("Authorization", "Bearer secret-orch")
	if !orchestratorRequestAllowed(bearer) {
		t.Fatal("bearer token must allow remote")
	}
}

func TestPerfOwnedAndEmptyOrgFailClosed(t *testing.T) {
	prevAuth := authEnforced
	prevQC := queryClient
	setAuthEnforced(true)
	queryClient = &ClickHouseQuery{}
	t.Cleanup(func() {
		setAuthEnforced(prevAuth)
		queryClient = prevQC
	})

	if got := perfOwnedAnd(nil); got != "" {
		t.Fatalf("nil request: %q", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/perf/runs/x", nil)
	if got := perfOwnedAnd(r); got != " AND (1=0)" {
		t.Fatalf("empty org under auth: %q", got)
	}
	r.Header.Set("X-Organization-ID", "all")
	if got := perfOwnedAnd(r); got != " AND (1=0)" {
		t.Fatalf("all org under auth: %q", got)
	}
	r.Header.Del("X-Organization-ID")
	r.Header.Set("X-Organization-ID", "")
	r.Header.Set("X-Project-ID", "infra")
	if got := perfOwnedAnd(r); got != " AND (1=0)" {
		t.Fatalf("empty org with project still fail-closed: %q", got)
	}
	r.Header.Set("X-Organization-ID", "nas")
	r.Header.Set("X-Project-ID", "infra")
	got := perfOwnedAnd(r)
	if got == "" || got == " AND (1=0)" {
		t.Fatalf("concrete tenant should scope, got %q", got)
	}
	if !strings.Contains(got, "nas") || !strings.Contains(got, "infra") {
		t.Fatalf("owned predicate should pin concrete tenant, got %q", got)
	}
}

func TestPerfOwnedAndForeignOrgFailClosed(t *testing.T) {
	prevAuth := authEnforced
	prevQC := queryClient
	setAuthEnforced(true)
	queryClient = &ClickHouseQuery{}
	t.Cleanup(func() {
		setAuthEnforced(prevAuth)
		queryClient = prevQC
	})

	// By-id reads use OwnedRowPredicate (write tenant) — foreign org header must
	// scope to that org (empty result set), never leak another tenant's rows.
	r := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios/sc-1", nil)
	r.Header.Set("X-Organization-ID", "foreign-org-xyz")
	r.Header.Set("X-Project-ID", "foreign-proj")
	got := perfOwnedAnd(r)
	if got == "" || got == " AND (1=0)" {
		// Empty org is fail-closed; a concrete foreign org must still emit a
		// tenant predicate so SQL cannot match home-tenant rows by accident.
		t.Fatalf("foreign org should emit owned predicate, got %q", got)
	}
	if !strings.Contains(got, "foreign-org-xyz") {
		t.Fatalf("predicate must pin foreign org (empty match), got %q", got)
	}
	if strings.Contains(got, "default-org") && !strings.Contains(got, "foreign-org-xyz") {
		t.Fatalf("must not fall back to default-org alone: %q", got)
	}

	ctx, _ := ExtractTenantContext(r, queryClient)
	pred := ctx.OwnedRowPredicate("")
	if !strings.Contains(pred, "foreign-org-xyz") || !strings.Contains(pred, "foreign-proj") {
		t.Fatalf("OwnedRowPredicate foreign pin: %q", pred)
	}
}
