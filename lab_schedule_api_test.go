package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fireRow(id, scenarioID, owner, outcome string, firedAt time.Time) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "scenario_id": scenarioID,
		"organization_id": "", "project_id": "",
		"fire_key": "at:" + firedAt.UTC().Format("20060102T150405Z"),
		"owner":    owner, "run_id": "run-" + id, "outcome": outcome,
		"vus": 10, "detail": "", "source": "opl-orchestrator",
		"next_fire_at": firedAt.Add(15 * time.Minute).Format("2006-01-02 15:04:05.000"),
		"fired_at":     firedAt.Format("2006-01-02 15:04:05.000"),
	}
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// TestScheduleHistoryEndpoint covers the audit endpoint: fires come back newest
// first, scoped to the scenario, with the lease owner that won each occurrence.
func TestScheduleHistoryEndpoint(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	f.ensureTable("load_schedule_fires")
	f.seed("load_schedule_fires",
		fireRow("f1", "scn-1", "replica-a", "fired", base),
		fireRow("f2", "scn-1", "replica-b", "dispatch_failed", base.Add(15*time.Minute)),
		fireRow("f3", "scn-2", "replica-a", "fired", base.Add(20*time.Minute)),
	)

	rec := httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/perf/scenarios/scn-1/schedule/history", nil), "scn-1")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	if body["ok"] != true {
		t.Fatalf("expected ok=true, got %v", body["ok"])
	}
	if body["scenario_id"] != "scn-1" {
		t.Fatalf("expected scenario_id scn-1, got %v", body["scenario_id"])
	}
	fires, _ := body["fires"].([]interface{})
	if len(fires) != 2 {
		t.Fatalf("expected 2 fires for scn-1 (the third belongs to scn-2), got %d", len(fires))
	}
	if got := int(getFloat64(body, "count")); got != 2 {
		t.Fatalf("expected count 2, got %d", got)
	}
	// Newest first.
	first, _ := fires[0].(map[string]interface{})
	if getString(first, "id") != "f2" {
		t.Fatalf("expected newest fire f2 first, got %v", getString(first, "id"))
	}
	if getString(first, "owner") != "replica-b" {
		t.Fatalf("history must carry the lease owner, got %v", getString(first, "owner"))
	}
	if getString(first, "outcome") != "dispatch_failed" {
		t.Fatalf("history must carry the outcome, got %v", getString(first, "outcome"))
	}
	if body["honesty"] == nil || body["honesty"] == "" {
		t.Fatalf("history response must keep its honesty string")
	}
}

// TestScheduleHistoryEndpointFilters covers the outcome and owner filters and the
// limit clamp.
func TestScheduleHistoryEndpointFilters(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	f.ensureTable("load_schedule_fires")
	f.seed("load_schedule_fires",
		fireRow("f1", "scn-1", "replica-a", "fired", base),
		fireRow("f2", "scn-1", "replica-b", "dispatch_failed", base.Add(15*time.Minute)),
		fireRow("f3", "scn-1", "replica-a", "fired", base.Add(30*time.Minute)),
	)

	rec := httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?outcome=fired", nil), "scn-1")
	fires, _ := decodeJSONBody(t, rec)["fires"].([]interface{})
	if len(fires) != 2 {
		t.Fatalf("outcome filter: expected 2 fired rows, got %d", len(fires))
	}

	rec = httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?owner=replica-b", nil), "scn-1")
	fires, _ = decodeJSONBody(t, rec)["fires"].([]interface{})
	if len(fires) != 1 {
		t.Fatalf("owner filter: expected 1 row, got %d", len(fires))
	}

	rec = httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?limit=1", nil), "scn-1")
	fires, _ = decodeJSONBody(t, rec)["fires"].([]interface{})
	if len(fires) != 1 {
		t.Fatalf("limit: expected 1 row, got %d", len(fires))
	}
}

// TestScheduleHistoryLimitClamp pins the bounds so a caller cannot ask for an
// unbounded scan.
func TestScheduleHistoryLimitClamp(t *testing.T) {
	cases := map[string]int{
		"":           50,
		"?limit=1":   1,
		"?limit=500": 500,
		"?limit=999": 500,
		"?limit=0":   50,
		"?limit=abc": 50,
		"?limit=-5":  50,
	}
	for q, want := range cases {
		got := scheduleHistoryLimit(httptest.NewRequest(http.MethodGet, "/x"+q, nil))
		if got != want {
			t.Fatalf("limit for %q: got %d want %d", q, got, want)
		}
	}
}

// TestScheduleHistoryEndpointMethodNotAllowed keeps the endpoint read-only.
func TestScheduleHistoryEndpointMethodNotAllowed(t *testing.T) {
	f := wireFakeClickHouse(t)
	f.ensureTable("load_schedule_fires")
	rec := httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodPost, "/x", nil), "scn-1")
	if rec.Code != 405 {
		t.Fatalf("expected 405 for POST, got %d", rec.Code)
	}
}

// TestScheduleHistoryMissingTableIsEmptyNot500 pins the pre-migration path: a
// deployment without the history table reports an empty, explained history
// rather than a 500.
func TestScheduleHistoryMissingTableIsEmptyNot500(t *testing.T) {
	wireFakeClickHouse(t) // no load_schedule_fires table registered
	rec := httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "scn-1")
	if rec.Code != 200 {
		t.Fatalf("expected 200 on a pre-migration deployment, got %d", rec.Code)
	}
	body := decodeJSONBody(t, rec)
	fires, _ := body["fires"].([]interface{})
	if len(fires) != 0 {
		t.Fatalf("expected an empty history, got %d rows", len(fires))
	}
	if body["honesty"] == nil {
		t.Fatalf("the empty-history response must say why it is empty")
	}
}

// TestScheduleHistoryNotReady pins the 503 when ClickHouse is not wired up.
func TestScheduleHistoryNotReady(t *testing.T) {
	prev := queryClient
	queryClient = nil
	t.Cleanup(func() { queryClient = prev })
	rec := httptest.NewRecorder()
	handlePerfScheduleHistory(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "scn-1")
	if rec.Code != 503 {
		t.Fatalf("expected 503 when not ready, got %d", rec.Code)
	}
}

// TestSchedulesListEndpoint covers the list view: every schedule with its
// server-computed next fire time, so the UI never guesses.
func TestSchedulesListEndpoint(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	scheduled := scenarioRow("scn-1", "nightly smoke", base)
	unscheduled := scenarioRow("scn-2", "manual only", base)
	unscheduled["schedule_json"] = "{}"
	f.seed("load_scenarios", scheduled, unscheduled)
	f.ensureTable("load_schedule_state", "load_schedule_leases")
	f.seed("load_schedule_state", map[string]interface{}{
		"scenario_id": "scn-1", "organization_id": "org", "project_id": "proj",
		"last_fired_at": base.Format("2006-01-02 15:04:05.000"),
		"next_fire_at":  base.Add(15 * time.Minute).Format("2006-01-02 15:04:05.000"),
		"last_run_id":   "run-1", "last_fire_key": "k1", "last_owner": "replica-a",
		"fire_count": 4, "updated_at": base.Format("2006-01-02 15:04:05.000"),
	})

	rec := httptest.NewRecorder()
	handlePerfSchedules(rec, httptest.NewRequest(http.MethodGet, "/api/perf/schedules", nil))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	list, _ := body["schedules"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("only the scheduled scenario should be listed, got %d", len(list))
	}
	entry, _ := list[0].(map[string]interface{})
	if getString(entry, "scenario_id") != "scn-1" {
		t.Fatalf("expected scn-1, got %v", getString(entry, "scenario_id"))
	}
	status, _ := entry["status"].(map[string]interface{})
	if status["next_fire_source"] != "state_row" {
		t.Fatalf("next fire should come from the state row, got %v", status["next_fire_source"])
	}
	if status["next_fire_at"] != base.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("unexpected next_fire_at %v", status["next_fire_at"])
	}
	if status["last_owner"] != "replica-a" {
		t.Fatalf("lease owner must be exposed on the list, got %v", status["last_owner"])
	}
	if int(getFloat64(status, "fire_count")) != 4 {
		t.Fatalf("expected fire_count 4, got %v", status["fire_count"])
	}
	if body["this_owner"] == nil || body["this_owner"] == "" {
		t.Fatalf("the list must name the process serving it")
	}
	if body["honesty"] == nil {
		t.Fatalf("the list must keep its honesty string")
	}
}

// TestSchedulesListRoutesHistory covers /api/perf/schedules/history.
func TestSchedulesListRoutesHistory(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	f.ensureTable("load_schedule_fires")
	f.seed("load_schedule_fires", fireRow("f1", "scn-1", "replica-a", "fired", base))
	rec := httptest.NewRecorder()
	handlePerfSchedules(rec, httptest.NewRequest(http.MethodGet, "/api/perf/schedules/history?scenario_id=scn-1", nil))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	fires, _ := decodeJSONBody(t, rec)["fires"].([]interface{})
	if len(fires) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(fires))
	}
}

// TestSchedulesListRejectsUnknownSubpath keeps the route surface tight.
func TestSchedulesListRejectsUnknownSubpath(t *testing.T) {
	rec := httptest.NewRecorder()
	handlePerfSchedules(rec, httptest.NewRequest(http.MethodGet, "/api/perf/schedules/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("expected 404 for an unknown subpath, got %d", rec.Code)
	}
}

// TestScenarioScheduleGetExposesNextFireAndLease covers requirement four end to
// end: GET on a scenario's schedule returns the computed next fire time and the
// lease owner, so the UI does not have to work either out.
func TestScenarioScheduleGetExposesNextFireAndLease(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	f.seed("load_scenarios", scenarioRow("scn-1", "nightly smoke", base))
	f.ensureTable("load_schedule_state", "load_schedule_leases")
	next := base.Add(15 * time.Minute)
	f.seed("load_schedule_state", map[string]interface{}{
		"scenario_id": "scn-1", "organization_id": "", "project_id": "",
		"last_fired_at": base.Format("2006-01-02 15:04:05.000"),
		"next_fire_at":  next.Format("2006-01-02 15:04:05.000"),
		"last_run_id":   "run-1", "last_fire_key": "k1", "last_owner": "replica-a",
		"fire_count": 2, "updated_at": base.Format("2006-01-02 15:04:05.000"),
	})
	// A live lease on the upcoming occurrence, held by another replica.
	fireKey := scheduleFireKey(map[string]interface{}{"every_minutes": 15}, scheduleState{NextFireAt: next}, time.Now().UTC())
	f.seed("load_schedule_leases", map[string]interface{}{
		"scenario_id": "scn-1", "fire_key": fireKey, "owner": "replica-b",
		"organization_id": "", "project_id": "",
		"claimed_at": time.Now().UTC().Format("2006-01-02 15:04:05.000"),
		"expires_at": time.Now().UTC().Add(5 * time.Minute).Format("2006-01-02 15:04:05.000"),
		"generation": 0, "released": 0, "run_id": "",
	})

	rec := httptest.NewRecorder()
	handlePerfScenarioSchedule(rec, httptest.NewRequest(http.MethodGet, "/api/perf/scenarios/scn-1/schedule", nil), "scn-1", "")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	status, _ := body["status"].(map[string]interface{})
	if status == nil {
		t.Fatalf("schedule GET must return a status block: %s", rec.Body.String())
	}
	if status["next_fire_at"] != next.Format(time.RFC3339) {
		t.Fatalf("expected server-side next_fire_at %v, got %v", next.Format(time.RFC3339), status["next_fire_at"])
	}
	lease, _ := body["lease"].(map[string]interface{})
	if lease == nil {
		t.Fatalf("schedule GET must return a lease block")
	}
	if lease["owner"] != "replica-b" {
		t.Fatalf("expected lease owner replica-b, got %v", lease["owner"])
	}
	if lease["held_by_this_process"] != false {
		t.Fatalf("a lease held elsewhere must not report as ours")
	}
	if body["honesty"] == nil {
		t.Fatalf("schedule GET must keep its honesty string")
	}
}

// TestScenarioScheduleUnknownSubpath keeps the schedule route surface tight.
func TestScenarioScheduleUnknownSubpath(t *testing.T) {
	f := wireFakeClickHouse(t)
	_ = f
	rec := httptest.NewRecorder()
	handlePerfScenarioSchedule(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "scn-1", "bogus")
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestScenarioScheduleMethodNotAllowed pins the allowed verbs.
func TestScenarioScheduleMethodNotAllowed(t *testing.T) {
	f := wireFakeClickHouse(t)
	_ = f
	rec := httptest.NewRecorder()
	handlePerfScenarioSchedule(rec, httptest.NewRequest(http.MethodDelete, "/x", nil), "scn-1", "")
	if rec.Code != 405 {
		t.Fatalf("expected 405 for DELETE, got %d", rec.Code)
	}
}
