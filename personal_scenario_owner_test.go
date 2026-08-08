package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	opentenant "github.com/TheGrimmChester/open-tenant-go"
)

// Personal HAR/upsert writes empty organization_id + user_id. Validate/get must
// use OwnedRowPredicate (user_id = owner), not fail-closed empty-org (1=0).
func TestPerfOwnedAndPersonalScopesByUserID(t *testing.T) {
	prevAuth := authEnforced
	prevQC := queryClient
	setAuthEnforced(true)
	queryClient = &ClickHouseQuery{}
	t.Cleanup(func() {
		setAuthEnforced(prevAuth)
		queryClient = prevQC
	})

	r := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios/x", nil)
	r.Header.Set(opentenant.HeaderTenantUserID, "alice")
	r.Header.Set("X-Project-ID", "admin-menu-component")
	got := perfOwnedAnd(r)
	if got == "" || got == " AND (1=0)" {
		t.Fatalf("personal owner must emit user_id predicate, got %q", got)
	}
	if !strings.Contains(got, "user_id = 'alice'") {
		t.Fatalf("personal owned predicate must pin user_id, got %q", got)
	}
}

func TestValidateFindsPersonalOwnedScenario(t *testing.T) {
	prevAuth := authEnforced
	setAuthEnforced(true)
	t.Cleanup(func() { setAuthEnforced(prevAuth) })

	srv, _, _ := countingTarget(t)
	f := wireFakeClickHouse(t)
	steps, _ := json.Marshal([]map[string]interface{}{
		{"type": "http", "name": "health", "method": "GET", "url": srv.URL + "/"},
	})
	ts := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05.000")
	f.seed("load_scenarios", map[string]interface{}{
		"id": "scn-personal-1", "organization_id": "", "project_id": "admin-menu-component",
		"user_id": "alice",
		"name": "personal-har", "target_url": srv.URL + "/", "method": "GET",
		"vus": 1, "duration_seconds": 30,
		"headers_json": "{}", "body": "", "thresholds_json": "{}",
		"steps_json": string(steps), "datasets_json": "{}", "sla_json": "{}",
		"schedule_json": "{}", "jmx_xml": "", "archived": 0,
		"updated_at": ts, "created_at": ts,
	})

	// Orphan row from before user_id (empty owner) must stay invisible.
	f.seed("load_scenarios", map[string]interface{}{
		"id": "scn-orphan", "organization_id": "", "project_id": "admin-menu-component",
		"user_id": "",
		"name": "orphan", "target_url": srv.URL + "/", "method": "GET",
		"vus": 1, "duration_seconds": 30,
		"headers_json": "{}", "body": "", "thresholds_json": "{}",
		"steps_json": string(steps), "datasets_json": "{}", "sla_json": "{}",
		"schedule_json": "{}", "jmx_xml": "", "archived": 0,
		"updated_at": ts, "created_at": ts,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/scn-personal-1/validate", nil)
	req.Header.Set(opentenant.HeaderTenantUserID, "alice")
	req.Header.Set("X-Project-ID", "admin-menu-component")
	req.Header.Set("X-User-Role", "editor")
	rec := httptest.NewRecorder()
	handlePerfScenarioValidate(rec, req, "scn-personal-1")
	if rec.Code != 200 {
		t.Fatalf("personal validate: got %d body=%s", rec.Code, rec.Body.String())
	}

	reqMiss := httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/scn-orphan/validate", nil)
	reqMiss.Header.Set(opentenant.HeaderTenantUserID, "alice")
	reqMiss.Header.Set("X-Project-ID", "admin-menu-component")
	reqMiss.Header.Set("X-User-Role", "editor")
	recMiss := httptest.NewRecorder()
	handlePerfScenarioValidate(recMiss, reqMiss, "scn-orphan")
	if recMiss.Code != 404 {
		t.Fatalf("orphan without user_id must 404, got %d body=%s", recMiss.Code, recMiss.Body.String())
	}
}

func TestHARImportPersistsPersonalUserID(t *testing.T) {
	prevAuth := authEnforced
	setAuthEnforced(true)
	t.Cleanup(func() { setAuthEnforced(prevAuth) })

	f := wireFakeClickHouse(t)
	har := []byte(`{"log":{"entries":[{"request":{"method":"GET","url":"https://example.com/a","headers":[]},"response":{"status":200}}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/import-har?name=personal-import", strings.NewReader(string(har)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(opentenant.HeaderTenantUserID, "alice")
	req.Header.Set("X-Project-ID", "admin-menu-component")
	req.Header.Set("X-User-Role", "editor")
	rec := httptest.NewRecorder()
	handlePerfImportHAR(rec, req)
	if rec.Code != 200 {
		t.Fatalf("import: got %d body=%s", rec.Code, rec.Body.String())
	}
	flushAsyncWrites(t, f, "load_scenarios", 1)
	rows := f.rows("load_scenarios")
	if len(rows) == 0 {
		t.Fatal("expected scenario row")
	}
	if got := getString(rows[len(rows)-1], "user_id"); got != "alice" {
		t.Fatalf("import must WriteOwner user_id=alice, got %q row=%v", got, rows[len(rows)-1])
	}
	if got := getString(rows[len(rows)-1], "organization_id"); got != "" {
		t.Fatalf("personal import must keep empty organization_id, got %q", got)
	}
}
