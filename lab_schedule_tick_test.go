package main

import (
	"testing"
	"time"
)

// End-to-end tests of the whole scheduler tick and the reaper, driven against
// the fake ClickHouse. Scenarios use "dispatch": false so the tick exercises
// everything except handing work to the container runner, which needs Docker.

func scheduledScenarioRow(id, name string, dispatch bool) map[string]interface{} {
	row := scenarioRow(id, name, time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC))
	row["organization_id"] = ""
	row["project_id"] = ""
	if dispatch {
		row["schedule_json"] = `{"enabled":true,"every_minutes":15}`
	} else {
		row["schedule_json"] = `{"enabled":true,"every_minutes":15,"dispatch":false}`
	}
	return row
}

// TestFireDueSchedulesTickFiresOnceThenAdvances is the whole loop: a due
// schedule fires exactly once, and the next tick finds it no longer due because
// the state row advanced. Before this change the "already fired" marker lived in
// the scenario row; now it lives in load_schedule_state.
func TestFireDueSchedulesTickFiresOnceThenAdvances(t *testing.T) {
	t.Setenv("OPL_SCHEDULER_LEASE_SETTLE_MS", "20")
	f := wireFakeClickHouse(t)
	f.seed("load_scenarios", scheduledScenarioRow("scn-1", "nightly smoke", false))
	f.ensureTable("load_schedule_state", "load_schedule_leases", "load_schedule_fires", "load_runs")

	first := fireDueSchedules()
	if first.Considered != 1 {
		t.Fatalf("expected 1 enabled schedule considered, got %d", first.Considered)
	}
	if first.Due != 1 {
		t.Fatalf("expected the schedule to be due, got due=%d", first.Due)
	}
	if first.Fired != 1 {
		t.Fatalf("expected exactly 1 fire, got %d (errors=%d lost=%d)", first.Fired, first.Errors, first.LostLease)
	}
	if first.LostLease != 0 || first.Errors != 0 {
		t.Fatalf("a single scheduler should lose no leases and hit no errors: %+v", first)
	}
	fired := first.Fires[0]
	if getString(fired, "outcome") != "recorded_no_dispatch" {
		t.Fatalf("expected the no-dispatch outcome, got %q", getString(fired, "outcome"))
	}
	if getString(fired, "run_id") == "" || getString(fired, "fire_key") == "" {
		t.Fatalf("a fire must report its run id and fire key: %v", fired)
	}

	flushAsyncWrites(t, f, "load_runs", 1)
	flushAsyncWrites(t, f, "load_schedule_state", 1)
	flushAsyncWrites(t, f, "load_schedule_fires", 1)

	// The scenario row must be untouched by the fire.
	if got := len(f.rows("load_scenarios")); got != 1 {
		t.Fatalf("the fire must not write load_scenarios, got %d rows", got)
	}
	// Exactly one run, one lease claim, one history row.
	if got := len(f.rows("load_runs")); got != 1 {
		t.Fatalf("expected 1 run row, got %d", got)
	}
	if got := len(f.rows("load_schedule_leases")); got != 1 {
		t.Fatalf("expected 1 lease claim, got %d", got)
	}
	if got := len(f.rows("load_schedule_fires")); got != 1 {
		t.Fatalf("expected 1 fire history row, got %d", got)
	}
	state := f.rows("load_schedule_state")[0]
	if int(getFloat64(state, "fire_count")) != 1 {
		t.Fatalf("expected fire_count 1, got %v", state["fire_count"])
	}
	if getString(state, "next_fire_at") == "" {
		t.Fatalf("the state row must record the next fire time")
	}

	// Second tick: the state row has advanced 15 minutes, so nothing is due.
	second := fireDueSchedules()
	if second.Due != 0 || second.Fired != 0 {
		t.Fatalf("the schedule must not fire again immediately: %+v", second)
	}
	if got := len(f.rows("load_runs")); got != 1 {
		t.Fatalf("a second tick must not create a second run, got %d", got)
	}
}

// TestFireDueSchedulesSkipsDisabledAndUnscheduled pins the filters.
func TestFireDueSchedulesSkipsDisabledAndUnscheduled(t *testing.T) {
	t.Setenv("OPL_SCHEDULER_LEASE_SETTLE_MS", "20")
	f := wireFakeClickHouse(t)
	disabled := scheduledScenarioRow("scn-off", "disabled", false)
	disabled["schedule_json"] = `{"enabled":false,"every_minutes":15}`
	none := scheduledScenarioRow("scn-none", "no schedule", false)
	none["schedule_json"] = "{}"
	broken := scheduledScenarioRow("scn-bad", "unparseable schedule", false)
	broken["schedule_json"] = `{not json`
	f.seed("load_scenarios", disabled, none, broken)
	f.ensureTable("load_schedule_state", "load_schedule_leases", "load_schedule_fires", "load_runs")

	res := fireDueSchedules()
	if res.Considered != 0 || res.Fired != 0 {
		t.Fatalf("disabled, unscheduled and unparseable schedules must all be skipped: %+v", res)
	}
	if got := len(f.rows("load_runs")); got != 0 {
		t.Fatalf("nothing should have been dispatched, got %d runs", got)
	}
}

// TestFireDueSchedulesLeaseAlreadyHeldStandsDown proves the tick honours a lease
// held by another replica: a live claim on the upcoming occurrence is enough to
// keep this process from firing.
func TestFireDueSchedulesLeaseAlreadyHeldStandsDown(t *testing.T) {
	t.Setenv("OPL_SCHEDULER_LEASE_SETTLE_MS", "20")
	f := wireFakeClickHouse(t)
	f.seed("load_scenarios", scheduledScenarioRow("scn-1", "nightly smoke", false))
	f.ensureTable("load_schedule_state", "load_schedule_leases", "load_schedule_fires", "load_runs")

	// Another replica already claimed the occurrence this tick will compute.
	now := time.Now().UTC()
	sched := map[string]interface{}{"enabled": true, "every_minutes": 15, "dispatch": false}
	fireKey := scheduleFireKey(sched, scheduleState{}, now)
	f.seed("load_schedule_leases", map[string]interface{}{
		"scenario_id": "scn-1", "fire_key": fireKey, "owner": "some-other-replica",
		"organization_id": "", "project_id": "",
		"claimed_at": now.Format("2006-01-02 15:04:05.000"),
		"expires_at": now.Add(5 * time.Minute).Format("2006-01-02 15:04:05.000"),
		"generation": 0, "released": 0, "run_id": "",
	})

	res := fireDueSchedules()
	if res.Due != 1 {
		t.Fatalf("the schedule should still be recognised as due, got %+v", res)
	}
	if res.Fired != 0 {
		t.Fatalf("a lease held elsewhere must stop this process firing, got %d fires", res.Fired)
	}
	if res.LostLease != 1 {
		t.Fatalf("expected 1 lost lease, got %d", res.LostLease)
	}
	if got := len(f.rows("load_runs")); got != 0 {
		t.Fatalf("standing down must not create a run, got %d", got)
	}
}

// TestSameProcessOwnerIsReentrantByDesign documents a real boundary of the lease
// rather than papering over it: the owner identity is per-process, so two
// concurrent ticks *inside one process* both see the lease as their own and both
// fire. That is why a process must run exactly one scheduler loop, which
// startPerfScheduler enforces with a sync.Once. Cross-process contention — the
// case the lease exists for — is covered by the concurrency tests.
func TestSameProcessOwnerIsReentrantByDesign(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	req := scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "same-owner",
		Now: now, TTL: time.Minute, Settle: testSettle,
	}
	a, err := claimScheduleFire(store, req)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	b, err := claimScheduleFire(store, req)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !a.Acquired || !b.Acquired {
		t.Fatalf("same-owner claims are re-entrant by design: a=%v b=%v", a.Acquired, b.Acquired)
	}
}

// TestReapFinishedRunsClosesStaleRun drives the reaper end to end: a run left
// 'running' past the deadline, with no containers known to this process, is
// closed as a terminal error carrying the reason.
func TestReapFinishedRunsClosesStaleRun(t *testing.T) {
	t.Setenv("OPL_RUN_REAP_GRACE_SEC", "60")
	t.Setenv("OPL_RUN_MAX_SEC", "600")
	f := wireFakeClickHouse(t)
	started := time.Now().UTC().Add(-2 * time.Hour)
	f.seed("load_runs", map[string]interface{}{
		"id": "run-stale", "organization_id": "", "project_id": "",
		"scenario_id": "scn-1", "status": "running", "vus": 10,
		"started_at":   started.Format("2006-01-02 15:04:05.000"),
		"finished_at":  started.Format("2006-01-02 15:04:05.000"),
		"summary_json": `{"scheduled":true}`, "error": "",
	})

	reaped := reapFinishedRuns()
	if len(reaped) != 1 {
		t.Fatalf("expected 1 reaped run, got %d: %v", len(reaped), reaped)
	}
	if getString(reaped[0], "run_id") != "run-stale" {
		t.Fatalf("unexpected run reaped: %v", reaped[0])
	}
	if getString(reaped[0], "status") != "error" {
		t.Fatalf("expected a terminal error status, got %v", reaped[0]["status"])
	}
	if getString(reaped[0], "detail") == "" {
		t.Fatalf("a reaped run must record why")
	}
	flushAsyncWrites(t, f, "load_runs", 2)
	rows := f.rows("load_runs")
	latest := rows[len(rows)-1]
	if getString(latest, "status") != "error" {
		t.Fatalf("the run row must be updated to a terminal status, got %q", getString(latest, "status"))
	}
	if getString(latest, "id") != "run-stale" {
		t.Fatalf("the reaper wrote the wrong run: %q", getString(latest, "id"))
	}
	// The original start time must be preserved, not reset to now.
	if getString(latest, "started_at") != started.Format("2006-01-02 15:04:05.000") {
		t.Fatalf("the reaper must preserve started_at, got %q", getString(latest, "started_at"))
	}
}

// TestReapFinishedRunsLeavesFreshRunAlone pins the grace window end to end.
func TestReapFinishedRunsLeavesFreshRunAlone(t *testing.T) {
	t.Setenv("OPL_RUN_REAP_GRACE_SEC", "300")
	t.Setenv("OPL_RUN_MAX_SEC", "600")
	f := wireFakeClickHouse(t)
	started := time.Now().UTC().Add(-10 * time.Second)
	f.seed("load_runs", map[string]interface{}{
		"id": "run-fresh", "organization_id": "", "project_id": "",
		"scenario_id": "scn-1", "status": "running", "vus": 10,
		"started_at":   started.Format("2006-01-02 15:04:05.000"),
		"finished_at":  started.Format("2006-01-02 15:04:05.000"),
		"summary_json": "{}", "error": "",
	})
	if reaped := reapFinishedRuns(); len(reaped) != 0 {
		t.Fatalf("a run inside the grace window must not be reaped, got %v", reaped)
	}
	if got := len(f.rows("load_runs")); got != 1 {
		t.Fatalf("no extra run row should have been written, got %d", got)
	}
}

// TestOrchestratorStatsSnapshot pins the state endpoint's payload, since it is
// how an operator tells a working orchestrator from a health-check stub.
func TestOrchestratorStatsSnapshot(t *testing.T) {
	stats := &orchestratorStats{StartedAt: time.Now().UTC().Add(-time.Minute)}
	stats.recordTick(scheduleTickResult{Considered: 3, Due: 2, Fired: 1, LostLease: 1, Owner: "replica-a"})
	stats.recordTick(scheduleTickResult{Considered: 3, Due: 0, Fired: 0})
	stats.recordReap([]map[string]interface{}{{"run_id": "run-1", "status": "error"}})

	snap := stats.snapshot()
	if snap["schedule_ticks"] != 2 {
		t.Fatalf("expected 2 ticks, got %v", snap["schedule_ticks"])
	}
	if snap["runs_dispatched"] != 1 {
		t.Fatalf("expected 1 dispatch, got %v", snap["runs_dispatched"])
	}
	if snap["leases_lost"] != 1 {
		t.Fatalf("expected 1 lost lease, got %v", snap["leases_lost"])
	}
	if snap["runs_reaped"] != 1 {
		t.Fatalf("expected 1 reaped run, got %v", snap["runs_reaped"])
	}
	if snap["reap_ticks"] != 1 {
		t.Fatalf("expected 1 reap tick, got %v", snap["reap_ticks"])
	}
	for _, key := range []string{"owner", "started_at", "uptime_seconds", "last_tick_at", "last_fire_at", "last_reap_at", "lease_ttl_seconds"} {
		if snap[key] == nil {
			t.Fatalf("snapshot is missing %q", key)
		}
	}
}
