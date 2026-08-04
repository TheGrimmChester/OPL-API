package main

import (
	"testing"
	"time"
)

// These tests pin the scheduling arithmetic that moved out of lab_extras.go into
// lab_schedule.go. The move was meant to be behaviour-preserving, and the helper
// extraction (scheduleEveryMinutes / scheduleDailyAt / parseDailyAt) is only safe
// if the observable results are unchanged — so the table below is deliberately
// written against the pre-move semantics, including the odd corners.

func TestScheduleIsDueBehaviourPreserved(t *testing.T) {
	cases := []struct {
		name  string
		sched map[string]interface{}
		now   time.Time
		want  bool
	}{{
		name:  "explicit next_fire_at in the past is due",
		sched: map[string]interface{}{"next_fire_at": "2026-08-04T09:00:00Z"},
		now:   time.Date(2026, 8, 4, 9, 0, 1, 0, time.UTC),
		want:  true,
	}, {
		name:  "explicit next_fire_at exactly now is due",
		sched: map[string]interface{}{"next_fire_at": "2026-08-04T09:00:00Z"},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "explicit next_fire_at in the future is not due",
		sched: map[string]interface{}{"next_fire_at": "2026-08-04T09:00:00Z"},
		now:   time.Date(2026, 8, 4, 8, 59, 59, 0, time.UTC),
		want:  false,
	}, {
		name:  "unparseable next_fire_at falls through to the interval",
		sched: map[string]interface{}{"next_fire_at": "not-a-time", "every_minutes": 10},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "interval with no last fire is due immediately",
		sched: map[string]interface{}{"every_minutes": 10},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "interval not yet elapsed",
		sched: map[string]interface{}{"every_minutes": 10, "last_fired_at": "2026-08-04T08:55:00Z"},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  false,
	}, {
		name:  "interval exactly elapsed is due",
		sched: map[string]interface{}{"every_minutes": 10, "last_fired_at": "2026-08-04T08:50:00Z"},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "interval_minutes is accepted as a synonym",
		sched: map[string]interface{}{"interval_minutes": 10, "last_fired_at": "2026-08-04T08:50:00Z"},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "unparseable last_fired_at with an interval is treated as due",
		sched: map[string]interface{}{"every_minutes": 10, "last_fired_at": "garbage"},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "daily at the matching minute is due",
		sched: map[string]interface{}{"daily_at": "03:30"},
		now:   time.Date(2026, 8, 4, 3, 30, 15, 0, time.UTC),
		want:  true,
	}, {
		name:  "daily outside the matching minute is not due",
		sched: map[string]interface{}{"daily_at": "03:30"},
		now:   time.Date(2026, 8, 4, 3, 31, 0, 0, time.UTC),
		want:  false,
	}, {
		name:  "daily already fired today is not due again",
		sched: map[string]interface{}{"daily_at": "03:30", "last_fired_at": "2026-08-04T03:30:02Z"},
		now:   time.Date(2026, 8, 4, 3, 30, 40, 0, time.UTC),
		want:  false,
	}, {
		name:  "daily fired yesterday is due today",
		sched: map[string]interface{}{"daily_at": "03:30", "last_fired_at": "2026-08-03T03:30:02Z"},
		now:   time.Date(2026, 8, 4, 3, 30, 40, 0, time.UTC),
		want:  true,
	}, {
		name:  "cron_time is accepted as a synonym for daily_at",
		sched: map[string]interface{}{"cron_time": "03:30"},
		now:   time.Date(2026, 8, 4, 3, 30, 0, 0, time.UTC),
		want:  true,
	}, {
		name:  "malformed daily is never due",
		sched: map[string]interface{}{"daily_at": "3h30"},
		now:   time.Date(2026, 8, 4, 3, 30, 0, 0, time.UTC),
		want:  false,
	}, {
		name:  "out-of-range daily hour is never due",
		sched: map[string]interface{}{"daily_at": "25:00"},
		now:   time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC),
		want:  false,
	}, {
		name:  "out-of-range daily minute is never due",
		sched: map[string]interface{}{"daily_at": "03:75"},
		now:   time.Date(2026, 8, 4, 3, 15, 0, 0, time.UTC),
		want:  false,
	}, {
		name:  "no interval and no daily is never due",
		sched: map[string]interface{}{"enabled": true},
		now:   time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		want:  false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduleIsDue(tc.sched, tc.now); got != tc.want {
				t.Fatalf("scheduleIsDue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextFireFromScheduleBehaviourPreserved(t *testing.T) {
	from := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		sched map[string]interface{}
		want  time.Time
	}{{
		name:  "interval adds the interval",
		sched: map[string]interface{}{"every_minutes": 15},
		want:  from.Add(15 * time.Minute),
	}, {
		name:  "interval_minutes synonym",
		sched: map[string]interface{}{"interval_minutes": 45},
		want:  from.Add(45 * time.Minute),
	}, {
		name:  "daily later today",
		sched: map[string]interface{}{"daily_at": "23:30"},
		want:  time.Date(2026, 8, 4, 23, 30, 0, 0, time.UTC),
	}, {
		name:  "daily already passed rolls to tomorrow",
		sched: map[string]interface{}{"daily_at": "03:30"},
		want:  time.Date(2026, 8, 5, 3, 30, 0, 0, time.UTC),
	}, {
		name:  "daily exactly now rolls to tomorrow",
		sched: map[string]interface{}{"daily_at": "09:00"},
		want:  time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC),
	}, {
		name:  "cron_time synonym",
		sched: map[string]interface{}{"cron_time": "23:30"},
		want:  time.Date(2026, 8, 4, 23, 30, 0, 0, time.UTC),
	}, {
		name:  "malformed daily falls back to 24h",
		sched: map[string]interface{}{"daily_at": "nope"},
		want:  from.Add(24 * time.Hour),
	}, {
		name:  "nothing configured falls back to 24h",
		sched: map[string]interface{}{},
		want:  from.Add(24 * time.Hour),
	}, {
		name:  "interval wins over daily",
		sched: map[string]interface{}{"every_minutes": 5, "daily_at": "03:30"},
		want:  from.Add(5 * time.Minute),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextFireFromSchedule(tc.sched, from); !got.Equal(tc.want) {
				t.Fatalf("nextFireFromSchedule = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestScheduleEnabledEncodings pins both encodings the field has always carried.
func TestScheduleEnabledEncodings(t *testing.T) {
	cases := []struct {
		sched map[string]interface{}
		want  bool
	}{
		{map[string]interface{}{"enabled": true}, true},
		{map[string]interface{}{"enabled": false}, false},
		{map[string]interface{}{"enabled": float64(1)}, true},
		{map[string]interface{}{"enabled": float64(0)}, false},
		{map[string]interface{}{"enabled": "1"}, true},
		{map[string]interface{}{}, false},
	}
	for _, tc := range cases {
		if got := scheduleEnabled(tc.sched); got != tc.want {
			t.Fatalf("scheduleEnabled(%v) = %v, want %v", tc.sched, got, tc.want)
		}
	}
}

// TestScheduleDueWithStatePrefersStateRow pins the new precedence: the state row
// decides when it exists, and schedule_json only decides when it does not.
func TestScheduleDueWithStatePrefersStateRow(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	// schedule_json says "long overdue"; the state row says "not yet".
	sched := map[string]interface{}{"every_minutes": 10, "last_fired_at": "2020-01-01T00:00:00Z"}
	st := scheduleState{NextFireAt: now.Add(5 * time.Minute)}
	if scheduleDueWithState(sched, st, now) {
		t.Fatalf("the state row must win over a stale schedule_json")
	}

	// State row overdue: due.
	if !scheduleDueWithState(sched, scheduleState{NextFireAt: now.Add(-time.Second)}, now) {
		t.Fatalf("an overdue state row must make the schedule due")
	}

	// Only last_fired_at in the state row: interval arithmetic against it.
	if scheduleDueWithState(map[string]interface{}{"every_minutes": 10},
		scheduleState{LastFiredAt: now.Add(-5 * time.Minute)}, now) {
		t.Fatalf("5 minutes into a 10 minute interval is not due")
	}
	if !scheduleDueWithState(map[string]interface{}{"every_minutes": 10},
		scheduleState{LastFiredAt: now.Add(-11 * time.Minute)}, now) {
		t.Fatalf("11 minutes into a 10 minute interval is due")
	}

	// Daily via the state row.
	daily := map[string]interface{}{"daily_at": "09:00"}
	if scheduleDueWithState(daily, scheduleState{LastFiredAt: now.Add(-time.Minute)}, now) {
		t.Fatalf("a daily schedule already fired today must not fire again")
	}
	if !scheduleDueWithState(daily, scheduleState{LastFiredAt: now.Add(-25 * time.Hour)}, now) {
		t.Fatalf("a daily schedule last fired yesterday must be due")
	}
	// Daily outside its minute.
	if scheduleDueWithState(map[string]interface{}{"daily_at": "03:30"},
		scheduleState{LastFiredAt: now.Add(-25 * time.Hour)}, now) {
		t.Fatalf("a daily schedule outside its minute must not be due")
	}
	// A state row with a last fire but no interval and no daily never fires.
	if scheduleDueWithState(map[string]interface{}{"enabled": true},
		scheduleState{LastFiredAt: now.Add(-100 * time.Hour)}, now) {
		t.Fatalf("no interval and no daily must never be due")
	}

	// Empty state row: fall back to schedule_json.
	if !scheduleDueWithState(sched, scheduleState{}, now) {
		t.Fatalf("with no state row the legacy schedule_json path must still apply")
	}
}

// --- Reaper policy ---

// TestReapDecision pins the reaper policy: never reap inside the grace window,
// close a run whose containers have all exited on their exit codes, and close a
// run past the hard deadline whatever the container layer says.
func TestReapDecision(t *testing.T) {
	cases := []struct {
		name       string
		in         reapInput
		wantReap   bool
		wantStatus string
	}{{
		name:     "inside the grace window is never reaped",
		in:       reapInput{AgeSeconds: 30, GraceSeconds: 120, MaxSeconds: 7200},
		wantReap: false,
	}, {
		name: "a just-dispatched run whose containers are not up yet is not reaped",
		in: reapInput{AgeSeconds: 5, GraceSeconds: 120, MaxSeconds: 7200,
			Registered: true, ContainersSeen: 0},
		wantReap: false,
	}, {
		name: "all containers exited cleanly closes the run as completed",
		in: reapInput{AgeSeconds: 300, GraceSeconds: 120, MaxSeconds: 7200,
			Registered: true, ContainersSeen: 2, ContainersRunning: 0},
		wantReap: true, wantStatus: "completed",
	}, {
		name: "a non-zero exit closes the run as failed",
		in: reapInput{AgeSeconds: 300, GraceSeconds: 120, MaxSeconds: 7200,
			Registered: true, ContainersSeen: 2, ContainersRunning: 0, WorstExitCode: 3},
		wantReap: true, wantStatus: "failed",
	}, {
		name: "a container still running is left alone below the deadline",
		in: reapInput{AgeSeconds: 300, GraceSeconds: 120, MaxSeconds: 7200,
			Registered: true, ContainersSeen: 2, ContainersRunning: 1},
		wantReap: false,
	}, {
		name: "a container still running past the deadline is closed as error",
		in: reapInput{AgeSeconds: 7300, GraceSeconds: 120, MaxSeconds: 7200,
			Registered: true, ContainersSeen: 2, ContainersRunning: 1},
		wantReap: true, wantStatus: "error",
	}, {
		name:     "an unregistered run is only closed on the deadline",
		in:       reapInput{AgeSeconds: 300, GraceSeconds: 120, MaxSeconds: 7200},
		wantReap: false,
	}, {
		name:     "an unregistered run past the deadline is closed as error",
		in:       reapInput{AgeSeconds: 7200, GraceSeconds: 120, MaxSeconds: 7200},
		wantReap: true, wantStatus: "error",
	}, {
		name:     "a zero deadline disables deadline reaping",
		in:       reapInput{AgeSeconds: 999999, GraceSeconds: 120, MaxSeconds: 0},
		wantReap: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail, reap := reapDecision(tc.in)
			if reap != tc.wantReap {
				t.Fatalf("reap = %v (%s), want %v", reap, detail, tc.wantReap)
			}
			if reap && status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", status, tc.wantStatus)
			}
			if reap && detail == "" {
				t.Fatalf("a reaped run must carry a reason")
			}
			if reap && !runStatusTerminal(status) {
				t.Fatalf("reaping must produce a terminal status, got %q", status)
			}
		})
	}
}

// TestOrchestratorTickIntervalFloor pins the tick floors so a misconfiguration
// cannot turn the loops into a busy spin.
func TestOrchestratorTickIntervalFloor(t *testing.T) {
	t.Setenv("OPL_TEST_TICK", "1")
	if got := orchestratorTickInterval("OPL_TEST_TICK", 30, 5); got != 5*time.Second {
		t.Fatalf("expected the 5s floor, got %v", got)
	}
	t.Setenv("OPL_TEST_TICK", "45")
	if got := orchestratorTickInterval("OPL_TEST_TICK", 30, 5); got != 45*time.Second {
		t.Fatalf("expected 45s, got %v", got)
	}
	if got := orchestratorTickInterval("OPL_TEST_TICK_UNSET", 30, 5); got != 30*time.Second {
		t.Fatalf("expected the 30s default, got %v", got)
	}
}

// TestScheduleLeaseTTLAndSettleConfig pins the lease knobs, including the
// documented escape hatch of a zero settle window.
func TestScheduleLeaseTTLAndSettleConfig(t *testing.T) {
	if got := scheduleLeaseTTL(); got != defaultScheduleLeaseTTL {
		t.Fatalf("default lease TTL = %v, want %v", got, defaultScheduleLeaseTTL)
	}
	t.Setenv("OPL_SCHEDULER_LEASE_SEC", "90")
	if got := scheduleLeaseTTL(); got != 90*time.Second {
		t.Fatalf("lease TTL override = %v, want 90s", got)
	}
	t.Setenv("OPL_SCHEDULER_LEASE_SEC", "0")
	if got := scheduleLeaseTTL(); got != defaultScheduleLeaseTTL {
		t.Fatalf("a zero TTL must fall back to the default, got %v", got)
	}

	if got := scheduleClaimSettle(); got != defaultScheduleClaimSettleMS*time.Millisecond {
		t.Fatalf("default settle = %v", got)
	}
	t.Setenv("OPL_SCHEDULER_LEASE_SETTLE_MS", "250")
	if got := scheduleClaimSettle(); got != 250*time.Millisecond {
		t.Fatalf("settle override = %v, want 250ms", got)
	}
	t.Setenv("OPL_SCHEDULER_LEASE_SETTLE_MS", "-5")
	if got := scheduleClaimSettle(); got != 0 {
		t.Fatalf("a negative settle must clamp to zero, got %v", got)
	}
}

// TestFireDueSchedulesWithoutClickHouseIsInert pins the safe default: an
// unwired process reports nothing rather than firing.
func TestFireDueSchedulesWithoutClickHouseIsInert(t *testing.T) {
	prevQ, prevW := queryClient, writer
	queryClient, writer = nil, nil
	t.Cleanup(func() { queryClient, writer = prevQ, prevW })
	res := fireDueSchedules()
	if res.Fired != 0 || res.Due != 0 || res.Considered != 0 {
		t.Fatalf("an unwired process must not fire: %+v", res)
	}
	if reapFinishedRuns() != nil {
		t.Fatalf("an unwired process must not reap")
	}
	if scheduleLeaseStoreFor() != nil {
		t.Fatalf("no lease store should be offered without ClickHouse")
	}
}
