package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Notification history — one row per channel attempt on a terminal run.
//
// Every attempt is recorded, including channels that were skipped because they
// are not configured. The history is the audit trail behind the honesty rule:
// a notification is never silently dropped.
//
// Result values:
//
//	sent    — the destination accepted the delivery
//	failed  — the channel is configured but the destination errored
//	logged  — mode=log, or email recipients without an SMTP relay (intentional no-send)
//	skipped — the channel is disabled or has no destination configured

func newNotifyEventID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("ntf-%d", time.Now().UnixNano())
	}
	return "ntf-" + hex.EncodeToString(buf)
}

// recordRunNotifyHistory persists one row per channel attempt.
func recordRunNotifyHistory(evt runNotifyEvent, results []runNotifyResult) {
	if writer == nil || len(results) == 0 {
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	mode := runNotifyMode()
	var buf strings.Builder
	for _, res := range results {
		row, err := json.Marshal(map[string]interface{}{
			"id":              newNotifyEventID(),
			"organization_id": evt.OrganizationID,
			"project_id":      evt.ProjectID,
			"run_id":          evt.RunID,
			"scenario_id":     evt.ScenarioID,
			"run_status":      evt.Status,
			"channel":         res.Channel,
			"result":          res.Status,
			"target":          truncateStr(res.Target, 400),
			"detail":          truncateStr(res.Detail, 1000),
			"mode":            mode,
			"source":          evt.Source,
			"created_at":      now,
		})
		if err != nil {
			continue
		}
		buf.Write(row)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		return
	}
	if !writer.insert("run_notifications", []byte(buf.String())) {
		log.Printf("[run-notify] history insert failed for run=%s (delivery results were logged above)", evt.RunID)
	}
}

// queryRunNotifyHistory reads recent attempts, honouring tenant scope and filters.
func queryRunNotifyHistory(r *http.Request, runID string, limit int) ([]map[string]interface{}, error) {
	if queryClient == nil {
		return nil, fmt.Errorf("not ready")
	}
	where := " WHERE 1=1" + tenantScopeSQL(r, queryClient, "")
	if runID != "" {
		where += fmt.Sprintf(" AND run_id = '%s'", escapeSQL(runID))
	}
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel"))); v != "" {
		where += fmt.Sprintf(" AND channel = '%s'", escapeSQL(v))
	}
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("result"))); v != "" {
		where += fmt.Sprintf(" AND result = '%s'", escapeSQL(v))
	}
	return queryClient.Query(fmt.Sprintf(`
		SELECT id, run_id, scenario_id, run_status, channel, result, target, detail, mode, source, created_at
		FROM `+chTable("run_notifications")+`%s
		ORDER BY created_at DESC, channel ASC
		LIMIT %d`, where, limit))
}

func notifyHistoryLimit(r *http.Request) int {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = clampInt(n, 1, 500)
		}
	}
	return limit
}

// handlePerfNotifications serves GET (history) and routes POST .../test.
func handlePerfNotifications(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/perf/notifications"), "/")
	if sub == "test" {
		handlePerfNotifyTest(w, r)
		return
	}
	if sub != "" {
		http.Error(w, "not found", 404)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	rows, err := queryRunNotifyHistory(r, strings.TrimSpace(r.URL.Query().Get("run_id")), notifyHistoryLimit(r))
	if err != nil {
		if err.Error() == "not ready" {
			http.Error(w, "not ready", 503)
			return
		}
		// A missing table on a pre-migration deployment is an empty history, not a 500.
		writeJSON(w, map[string]interface{}{
			"ok": true, "notifications": []interface{}{}, "count": 0,
			"run_notify": runNotifyStatusInfo(),
			"honesty":    "No notification history available yet (table not initialised on this deployment).",
		})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"notifications": rows,
		"count":         len(rows),
		"run_notify":    runNotifyStatusInfo(),
		"honesty":       "One row per channel attempt. skipped = channel not configured (reason in detail); logged = intentional no-send.",
	})
}

// handlePerfRunNotifications serves GET /api/perf/runs/{id}/notifications.
func handlePerfRunNotifications(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	rows, err := queryRunNotifyHistory(r, runID, notifyHistoryLimit(r))
	if err != nil {
		if err.Error() == "not ready" {
			http.Error(w, "not ready", 503)
			return
		}
		rows = []map[string]interface{}{}
	}
	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"run_id":        runID,
		"notifications": rows,
		"count":         len(rows),
		"run_notify":    runNotifyStatusInfo(),
		"honesty":       "Per-channel delivery attempts for this run. An empty list means the run never reached a notified terminal status.",
	})
}

// handlePerfNotifyTest fires a synthetic terminal-run event through every channel
// and returns the per-channel result. It bypasses the status filter and the
// per-run dedup so operators can verify wiring without launching a load run.
func handlePerfNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	var body struct {
		RunID      string `json:"run_id"`
		ScenarioID string `json:"scenario_id"`
		Status     string `json:"status"`
	}
	if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	status := strings.ToLower(strings.TrimSpace(body.Status))
	if status == "" {
		status = "failed"
	}
	runID := strings.TrimSpace(body.RunID)
	if runID == "" {
		runID = "notify-test-" + time.Now().UTC().Format("20060102T150405Z")
	}
	evt := runNotifyEvent{
		RunID:          runID,
		ScenarioID:     strings.TrimSpace(body.ScenarioID),
		OrganizationID: org,
		ProjectID:      proj,
		Status:         status,
		Source:         "notify-test",
		FinishedAt:     time.Now().UTC().Format(time.RFC3339),
		Summary:        map[string]interface{}{"p50_ms": 0, "p95_ms": 0, "p99_ms": 0, "error_rate": 0, "requests": 0},
	}
	results := deliverRunNotifyAll(evt)
	recordRunNotifyHistory(evt, results)
	out := make([]map[string]interface{}, 0, len(results))
	for _, res := range results {
		entry := map[string]interface{}{"channel": res.Channel, "result": res.Status}
		if res.Target != "" {
			entry["target"] = res.Target
		}
		if res.Detail != "" {
			entry["detail"] = res.Detail
		}
		out = append(out, entry)
	}
	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"run_id":     runID,
		"status":     status,
		"results":    out,
		"run_notify": runNotifyStatusInfo(),
		"honesty":    "Synthetic terminal-run event with zeroed metrics — proves channel wiring only, not a real load run.",
	})
}
