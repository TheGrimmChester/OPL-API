package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// N5 — validate is admin-gated; export-jmx stays viewable (authView only).
func TestValidateRequiresAdminRole(t *testing.T) {
	prev := authEnforced
	authEnforced = true
	t.Cleanup(func() { authEnforced = prev })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/scn-role/validate", nil)
	req.Header.Set("X-User-Role", "viewer")
	handlePerfScenarioSubroutes(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer validate: got %d want 403 body=%s", rec.Code, rec.Body.String())
	}

	recAdmin := httptest.NewRecorder()
	reqAdmin := httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/scn-role/validate", nil)
	reqAdmin.Header.Set("X-User-Role", "admin")
	handlePerfScenarioSubroutes(recAdmin, reqAdmin)
	if recAdmin.Code == http.StatusForbidden {
		t.Fatalf("admin validate must not be blocked by role gate: body=%s", recAdmin.Body.String())
	}
}

func TestExportJMXAllowsViewer(t *testing.T) {
	prev := authEnforced
	authEnforced = true
	t.Cleanup(func() { authEnforced = prev })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/perf/scenarios/scn-role/export-jmx", nil)
	req.Header.Set("X-User-Role", "viewer")
	handlePerfScenarioSubroutes(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("viewer export-jmx must not hit admin gate: body=%s", rec.Body.String())
	}
}
