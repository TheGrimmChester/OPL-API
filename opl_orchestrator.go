package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	openjob "github.com/TheGrimmChester/open-job-go"
)

// opl-orchestrator: dispatch scheduled runs and reap finished ones.
//
// It shares the lease table with opl-api, so running both is safe — whichever
// reaches an occurrence first wins its lease and the other stands down. With
// only one process running, that process wins every lease and behaviour matches
// the single in-process scheduler that came before.

const (
	defaultOrchestratorTickSec = 30
	defaultReapTickSec         = 60
	defaultRunGraceSec         = 120
	defaultRunMaxSec           = 7200
)

// orchestratorStats is the running tally exposed on /api/health and
// /api/orchestrator/state — the honest answer to "is this thing doing anything?".
type orchestratorStats struct {
	mu           sync.Mutex
	StartedAt    time.Time
	Ticks        int
	Fired        int
	LostLease    int
	TickErrors   int
	ReapTicks    int
	Reaped       int
	LastTickAt   time.Time
	LastFireAt   time.Time
	LastReapAt   time.Time
	LastTick     scheduleTickResult
	LastReapRows []map[string]interface{}
}

var orchStats = &orchestratorStats{StartedAt: time.Now().UTC()}

func (s *orchestratorStats) recordTick(res scheduleTickResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Ticks++
	s.Fired += res.Fired
	s.LostLease += res.LostLease
	s.TickErrors += res.Errors
	s.LastTickAt = time.Now().UTC()
	if res.Fired > 0 {
		s.LastFireAt = s.LastTickAt
	}
	s.LastTick = res
}

func (s *orchestratorStats) recordReap(rows []map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReapTicks++
	s.Reaped += len(rows)
	s.LastReapAt = time.Now().UTC()
	if len(rows) > 0 {
		s.LastReapRows = rows
	}
}

func (s *orchestratorStats) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]interface{}{
		"owner":             scheduleOwner(),
		"started_at":        s.StartedAt.Format(time.RFC3339),
		"uptime_seconds":    int(time.Since(s.StartedAt).Seconds()),
		"schedule_ticks":    s.Ticks,
		"runs_dispatched":   s.Fired,
		"leases_lost":       s.LostLease,
		"tick_errors":       s.TickErrors,
		"reap_ticks":        s.ReapTicks,
		"runs_reaped":       s.Reaped,
		"lease_ttl_seconds": int(scheduleLeaseTTL().Seconds()),
	}
	if !s.LastTickAt.IsZero() {
		out["last_tick_at"] = s.LastTickAt.Format(time.RFC3339)
		out["last_tick"] = s.LastTick
	}
	if !s.LastFireAt.IsZero() {
		out["last_fire_at"] = s.LastFireAt.Format(time.RFC3339)
	}
	if !s.LastReapAt.IsZero() {
		out["last_reap_at"] = s.LastReapAt.Format(time.RFC3339)
	}
	if len(s.LastReapRows) > 0 {
		out["last_reaped"] = s.LastReapRows
	}
	return out
}

func orchestratorTickInterval(key string, defSec, minSec int) time.Duration {
	sec := atoiDefault(envOr(key, ""), defSec)
	if sec < minSec {
		sec = minSec
	}
	return time.Duration(sec) * time.Second
}

func runOPLOrchestrator() {
	setScheduleRole("opl-orchestrator")
	// Default bind is loopback-only. Override with ORCHESTRATOR_LISTEN_ADDR (e.g.
	// ":8097") only when the process is on a private network and gated below.
	addr := envOr("ORCHESTRATOR_LISTEN_ADDR", "127.0.0.1:8097")
	tag := envOr("OPL_RUNNER_TAG", "smoke")
	chURL := envOr("CLICKHOUSE_URL", "http://127.0.0.1:8123")

	// The orchestrator needs the same ClickHouse wiring as opl-api. Without it,
	// all it can do is answer health checks — which is all it used to do.
	writer = NewClickHouseWriter(chURL, 100)
	queryClient = NewClickHouseQuery(chURL)
	ensureClickHouseDatabase(queryClient)
	ensurePerfLabSchema(queryClient)

	dispatchOn := !envFlagOn("OPL_ORCHESTRATOR_DISPATCH_DISABLE") && !envFlagOn("OPA_PERF_SCHEDULER_DISABLE")
	reapOn := !envFlagOn("OPL_ORCHESTRATOR_REAP_DISABLE")

	if dispatchOn {
		go orchestratorDispatchLoop()
	} else {
		log.Printf("[orchestrator] dispatch disabled — this process will not fire schedules")
	}
	if reapOn {
		go orchestratorReapLoop()
	} else {
		log.Printf("[orchestrator] reaper disabled — finished runs stay 'running' until something else closes them")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"status":       "ok",
			"service":      "opl-orchestrator",
			"version":      buildVersion,
			"runners":      []string{openjob.RunnerImage("opl", "jmeter", tag)},
			"database":     clickHouseDatabase(),
			"dispatch":     dispatchOn,
			"reap":         reapOn,
			"orchestrator": orchStats.snapshot(),
			"honesty": "Dispatches leased scheduled runs and reaps finished ones. Sharing the lease table with opl-api " +
				"means both may run without double-firing. Container-level reaping only sees containers this process " +
				"dispatched; runs dispatched elsewhere are closed on the max-runtime deadline instead.",
		})
	})
	// Non-health routes require loopback client or OPL_ORCHESTRATOR_TOKEN —
	// schedule listing must not be an unauthenticated IDOR surface.
	mux.HandleFunc("/api/orchestrator/state", orchestratorInternal(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"ok":           true,
			"orchestrator": orchStats.snapshot(),
			"honesty":      "Counters are per-process and reset on restart; the durable audit trail is load_schedule_fires and load_schedule_leases.",
		})
	}))
	mux.HandleFunc("/api/orchestrator/schedules", orchestratorInternal(handlePerfSchedules))
	mux.HandleFunc("/api/perf/schedules", orchestratorInternal(handlePerfSchedules))
	log.Printf("opl-orchestrator listening on %s owner=%s dispatch=%v reap=%v (one container per load run); non-health routes require loopback or OPL_ORCHESTRATOR_TOKEN",
		addr, scheduleOwner(), dispatchOn, reapOn)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// orchestratorInternal gates diagnostic/schedule HTTP to loopback callers or a
// shared bearer/token header. Health stays public for probes.
func orchestratorInternal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if orchestratorRequestAllowed(r) {
			next(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func orchestratorRequestAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}
	want := strings.TrimSpace(envOr("OPL_ORCHESTRATOR_TOKEN", ""))
	if want != "" {
		got := strings.TrimSpace(r.Header.Get("X-OPL-Orchestrator-Token"))
		if got == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			}
		}
		if got != "" && got == want {
			return true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func orchestratorDispatchLoop() {
	interval := orchestratorTickInterval("OPL_ORCHESTRATOR_TICK_SEC", defaultOrchestratorTickSec, 5)
	log.Printf("[orchestrator] dispatch tick every %s owner=%s", interval, scheduleOwner())
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		orchStats.recordTick(fireDueSchedules())
	}
}

func orchestratorReapLoop() {
	interval := orchestratorTickInterval("OPL_ORCHESTRATOR_REAP_TICK_SEC", defaultReapTickSec, 5)
	log.Printf("[orchestrator] reap tick every %s", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		orchStats.recordReap(reapFinishedRuns())
	}
}

// --- Reaper ---

// reapInput is everything the reaper knows about one still-running run.
type reapInput struct {
	RunID string
	// AgeSeconds is how long the run has been in 'running'.
	AgeSeconds int
	// GraceSeconds is the minimum age before a run is eligible at all, so a run
	// dispatched moments ago is never reaped out from under itself.
	GraceSeconds int
	// MaxSeconds is the hard deadline after which a run is closed regardless of
	// what the container layer reports.
	MaxSeconds int
	// Registered is true when this process dispatched the run and therefore
	// holds its container names in memory.
	Registered bool
	// ContainersSeen counts containers that could actually be inspected.
	ContainersSeen int
	// ContainersRunning counts inspected containers still running.
	ContainersRunning int
	// WorstExitCode is the highest exit code among the finished containers.
	WorstExitCode int
}

// reapDecision decides what to do with one long-running run. Pure, so the policy
// is unit-testable without Docker or ClickHouse.
//
// Order matters: the grace window wins over everything (never reap a run that
// was just dispatched), then a fully-exited container set closes the run on its
// exit codes, then the hard deadline closes runs whose containers this process
// cannot see.
func reapDecision(in reapInput) (status, detail string, reap bool) {
	if in.AgeSeconds < in.GraceSeconds {
		return "", "within grace window", false
	}
	if in.Registered && in.ContainersSeen > 0 && in.ContainersRunning == 0 {
		if in.WorstExitCode == 0 {
			return "completed", fmt.Sprintf("reaped: all %d engine containers exited 0", in.ContainersSeen), true
		}
		return "failed", fmt.Sprintf("reaped: engine container exited %d", in.WorstExitCode), true
	}
	if in.MaxSeconds > 0 && in.AgeSeconds >= in.MaxSeconds {
		if in.Registered {
			return "error", fmt.Sprintf("reaped: exceeded max runtime %ds with engine containers still reported running", in.MaxSeconds), true
		}
		return "error", fmt.Sprintf("reaped: exceeded max runtime %ds and no engine containers are known to this process", in.MaxSeconds), true
	}
	return "", "still running", false
}

// reapFinishedRuns closes out runs stuck in 'running'.
//
// Nothing closed them before: a crashed engine left a run running forever, so
// the run list lied and every later scheduled fire looked like it overlapped a
// live run.
func reapFinishedRuns() []map[string]interface{} {
	if queryClient == nil || writer == nil {
		return nil
	}
	grace := atoiDefault(envOr("OPL_RUN_REAP_GRACE_SEC", ""), defaultRunGraceSec)
	maxSec := atoiDefault(envOr("OPL_RUN_MAX_SEC", ""), defaultRunMaxSec)
	rows, err := queryClient.Query(`
		SELECT id, organization_id, project_id, scenario_id, status, vus, summary_json, error,
			toUnixTimestamp64Milli(started_at) AS started_ms
		FROM ` + chTable("load_runs") + ` FINAL
		WHERE status = 'running'
		ORDER BY started_at ASC LIMIT 200`)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	out := []map[string]interface{}{}
	for _, row := range rows {
		runID := getString(row, "id")
		if runID == "" {
			continue
		}
		started := scheduleTimeFromMillis(getFloat64(row, "started_ms"))
		if started.IsZero() {
			started = now
		}
		in := reapInput{
			RunID:        runID,
			AgeSeconds:   int(now.Sub(started).Seconds()),
			GraceSeconds: grace,
			MaxSeconds:   maxSec,
		}
		if st := lookupRunContainers(runID); st != nil {
			in.Registered = true
			for _, name := range st.Containers {
				snap := dockerContainerSnapshot(name)
				if found, _ := snap["found"].(bool); !found {
					continue
				}
				in.ContainersSeen++
				if running, _ := snap["running"].(bool); running {
					in.ContainersRunning++
					continue
				}
				if code := int(getFloat64(snap, "exit_code")); code > in.WorstExitCode {
					in.WorstExitCode = code
				}
			}
		}
		status, detail, reap := reapDecision(in)
		if !reap {
			continue
		}
		org := nz(getString(row, "organization_id"), defaultOrgID)
		proj := nz(getString(row, "project_id"), defaultProjectID)
		summary := nz(getString(row, "summary_json"), "{}")
		ts := now.Format("2006-01-02 15:04:05.000")
		payload, err := json.Marshal(map[string]interface{}{
			"id": runID, "organization_id": org, "project_id": proj,
			"scenario_id": getString(row, "scenario_id"), "status": status,
			"vus": int(getFloat64(row, "vus")), "started_at": started.Format("2006-01-02 15:04:05.000"),
			"finished_at": ts, "summary_json": summary, "error": detail,
		})
		if err != nil {
			continue
		}
		writer.insertAsync("load_runs", append(payload, '\n'))
		notifyRunTerminal(runNotifyEvent{
			RunID: runID, ScenarioID: getString(row, "scenario_id"),
			OrganizationID: org, ProjectID: proj, Status: status,
			VUs: int(getFloat64(row, "vus")), Error: detail,
			Summary: parseSummaryLoose(summary), FinishedAt: ts, Source: "reaper",
		})
		clearRunContainers(runID)
		log.Printf("[reaper] run=%s -> %s (%s)", runID, status, detail)
		out = append(out, map[string]interface{}{
			"run_id": runID, "status": status, "detail": detail,
			"age_seconds": in.AgeSeconds, "registered": in.Registered,
		})
	}
	return out
}
