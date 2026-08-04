package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- Custom load curve (inflection points → ThreadGroup peak/ramp/duration) ---

type loadCurvePoint struct {
	TSec int `json:"t"`   // seconds from test start
	VUs  int `json:"vus"` // concurrent VUs at that time
}

// normalizeLoadCurve sorts, clamps, and ensures a start point.
func normalizeLoadCurve(points []loadCurvePoint) []loadCurvePoint {
	if len(points) == 0 {
		return nil
	}
	out := make([]loadCurvePoint, 0, len(points))
	for _, p := range points {
		if p.TSec < 0 {
			p.TSec = 0
		}
		p.VUs = clampPerfVUs(p.VUs)
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1].TSec > out[j].TSec {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	if out[0].TSec != 0 {
		out = append([]loadCurvePoint{{TSec: 0, VUs: 0}}, out...)
	}
	return out
}

// applyLoadCurveToSchedule sets peak VUs, duration, and ramp from a custom curve.
// Honesty: OPL maps the curve to ThreadGroup num_threads + duration + ramp_time — not ArrivalsThreadGroup.
func applyLoadCurveToSchedule(curve []loadCurvePoint, sched map[string]interface{}) (vus, durationSec int, honesty string) {
	curve = normalizeLoadCurve(curve)
	if len(curve) == 0 {
		return 0, 0, ""
	}
	peak, dur, rampToPeak := 0, 0, 0
	for _, p := range curve {
		if p.VUs > peak {
			peak = p.VUs
			rampToPeak = p.TSec
		}
		if p.TSec > dur {
			dur = p.TSec
		}
	}
	if peak <= 0 {
		peak = 1
	}
	if dur <= 0 {
		dur = 60
	}
	if sched == nil {
		sched = map[string]interface{}{}
	}
	sched["policy"] = "custom"
	sched["curve"] = curve
	sched["ramp_seconds"] = rampToPeak
	sched["peak_vus"] = peak
	sched["duration_seconds"] = dur
	return peak, dur, "Custom load curve → peak VUs + duration + ramp_seconds for JMeter ThreadGroup (≠ Arrivals / point-accurate injectors)."
}

func parseCurveFromSchedule(sched map[string]interface{}) []loadCurvePoint {
	raw, ok := sched["curve"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var pts []loadCurvePoint
	if json.Unmarshal(b, &pts) != nil {
		return nil
	}
	return normalizeLoadCurve(pts)
}

// --- Scenario scheduler (interval / daily) ---

var (
	schedulerOnce sync.Once
)

func startPerfScheduler() {
	schedulerOnce.Do(func() {
		if envFlagOn("OPA_PERF_SCHEDULER_DISABLE") {
			log.Printf("[scheduler] disabled via OPA_PERF_SCHEDULER_DISABLE")
			return
		}
		interval := time.Duration(atoiDefault(envOr("OPA_PERF_SCHEDULER_TICK_SEC", "60"), 60)) * time.Second
		if interval < 15*time.Second {
			interval = 15 * time.Second
		}
		go func() {
			log.Printf("[scheduler] tick every %s — fires schedule_json.enabled scenarios", interval)
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				fireDueSchedules()
			}
		}()
	})
}

func fireDueSchedules() {
	if queryClient == nil || writer == nil {
		return
	}
	rows, err := queryClient.Query(`
		SELECT id, name, organization_id, project_id, vus, duration_seconds, schedule_json, target_url,
			method, headers_json, body, thresholds_json, steps_json, datasets_json, sla_json, jmx_xml
		FROM ` + chTable("load_scenarios") + ` FINAL
		WHERE coalesce(archived, 0) = 0
		ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, row := range rows {
		schedRaw := getString(row, "schedule_json")
		if schedRaw == "" || schedRaw == "{}" {
			continue
		}
		var sched map[string]interface{}
		if json.Unmarshal([]byte(schedRaw), &sched) != nil {
			continue
		}
		enabled, _ := sched["enabled"].(bool)
		if !enabled && getFloat64(sched, "enabled") != 1 {
			continue
		}
		if !scheduleIsDue(sched, now) {
			continue
		}
		org := nz(getString(row, "organization_id"), defaultOrgID)
		proj := nz(getString(row, "project_id"), defaultProjectID)
		scnID := getString(row, "id")
		vus := int(getFloat64(row, "vus"))
		if v := int(getFloat64(sched, "vus")); v > 0 {
			vus = v
		}
		workers := int(getFloat64(sched, "workers"))
		policy, _ := sched["policy"].(string)
		profile, _ := sched["profile"].(string)
		dispatch := true
		if v, ok := sched["dispatch"].(bool); ok {
			dispatch = v
		}
		if curve := parseCurveFromSchedule(sched); len(curve) > 0 {
			peak, _, _ := applyLoadCurveToSchedule(curve, sched)
			if peak > 0 {
				vus = peak
			}
		}
		_, resolvedSched, _ := resolveLoadPolicy(policy, profile, sched)
		vus = clampPerfVUs(vus)
		runID := loadID("run", org, proj, scnID, fmt.Sprintf("sched-%d", now.UnixNano()))
		ts := now.Format("2006-01-02 15:04:05.000")
		payload, _ := json.Marshal(map[string]interface{}{
			"id": runID, "organization_id": org, "project_id": proj,
			"scenario_id": scnID, "status": "running", "vus": vus,
			"started_at": ts, "finished_at": ts, "summary_json": `{"scheduled":true}`, "error": "",
		})
		writer.insertAsync("load_runs", append(payload, '\n'))
		if dispatch {
			dispatchInfo := dispatchJMeterRunScaled(scnID, runID, vus, workers, org, proj)
			runStatus, runErr := initialLoadRunStatus(true, dispatchInfo)
			if names := containerNamesFromAny(dispatchInfo["containers"]); len(names) > 0 {
				registerRunContainers(runID, names, getString(dispatchInfo, "mode"), getString(dispatchInfo, "image"), int(getFloat64(dispatchInfo, "workers")))
			}
			if runStatus != "running" {
				sum, _ := json.Marshal(map[string]interface{}{"scheduled": true, "dispatch_error": runErr, "policy": resolvedSched["policy"]})
				fix, _ := json.Marshal(map[string]interface{}{
					"id": runID, "organization_id": org, "project_id": proj,
					"scenario_id": scnID, "status": runStatus, "vus": vus,
					"started_at": ts, "finished_at": ts, "summary_json": string(sum), "error": runErr,
				})
				writer.insertAsync("load_runs", append(fix, '\n'))
				if runStatusTerminal(runStatus) {
					notifyRunTerminal(runNotifyEvent{
						RunID: runID, ScenarioID: scnID, OrganizationID: org, ProjectID: proj,
						Status: runStatus, VUs: vus, Error: runErr,
						Summary: map[string]interface{}{"scheduled": true, "dispatch_error": runErr},
						FinishedAt: ts, Source: "scheduler",
					})
				}
			}
		}
		sched["last_fired_at"] = now.Format(time.RFC3339)
		sched["next_fire_at"] = nextFireFromSchedule(sched, now).Format(time.RFC3339)
		bumpScenarioSchedule(scnID, org, proj, row, sched)
		log.Printf("[scheduler] fired scenario=%s run=%s vus=%d", scnID, runID, vus)
	}
}

func scheduleIsDue(sched map[string]interface{}, now time.Time) bool {
	if next := strings.TrimSpace(getString(sched, "next_fire_at")); next != "" {
		if t, err := time.Parse(time.RFC3339, next); err == nil {
			return !now.Before(t)
		}
	}
	every := int(getFloat64(sched, "every_minutes"))
	if every <= 0 {
		every = int(getFloat64(sched, "interval_minutes"))
	}
	last := strings.TrimSpace(getString(sched, "last_fired_at"))
	if every > 0 {
		if last == "" {
			return true
		}
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			return now.Sub(t) >= time.Duration(every)*time.Minute
		}
		return true
	}
	daily := strings.TrimSpace(firstNonEmpty(getString(sched, "daily_at"), getString(sched, "cron_time")))
	if daily == "" {
		return false
	}
	parts := strings.Split(daily, ":")
	if len(parts) != 2 {
		return false
	}
	hh, mm := atoiDefault(parts[0], -1), atoiDefault(parts[1], -1)
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return false
	}
	if now.Hour() != hh || now.Minute() != mm {
		return false
	}
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && t.UTC().Format("2006-01-02") == now.Format("2006-01-02") {
			return false
		}
	}
	return true
}

func nextFireFromSchedule(sched map[string]interface{}, from time.Time) time.Time {
	every := int(getFloat64(sched, "every_minutes"))
	if every <= 0 {
		every = int(getFloat64(sched, "interval_minutes"))
	}
	if every > 0 {
		return from.Add(time.Duration(every) * time.Minute)
	}
	daily := strings.TrimSpace(firstNonEmpty(getString(sched, "daily_at"), getString(sched, "cron_time")))
	if daily != "" {
		parts := strings.Split(daily, ":")
		if len(parts) == 2 {
			hh, mm := atoiDefault(parts[0], 0), atoiDefault(parts[1], 0)
			next := time.Date(from.Year(), from.Month(), from.Day(), hh, mm, 0, 0, time.UTC)
			if !next.After(from) {
				next = next.Add(24 * time.Hour)
			}
			return next
		}
	}
	return from.Add(24 * time.Hour)
}

func bumpScenarioSchedule(id, org, proj string, row, sched map[string]interface{}) {
	schedJSON, _ := json.Marshal(sched)
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	sc := loadScenarioMapForTenant(id, org, proj)
	if sc == nil {
		sc = row
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": getString(sc, "name"), "target_url": getString(sc, "target_url"),
		"method": nz(getString(sc, "method"), "GET"),
		"vus": int(getFloat64(sc, "vus")), "duration_seconds": int(getFloat64(sc, "duration_seconds")),
		"headers_json": nz(getString(sc, "headers_json"), "{}"), "body": getString(sc, "body"),
		"thresholds_json": nz(getString(sc, "thresholds_json"), "{}"),
		"steps_json": nz(getString(sc, "steps_json"), "[]"), "datasets_json": nz(getString(sc, "datasets_json"), "{}"),
		"sla_json": nz(getString(sc, "sla_json"), "{}"), "schedule_json": string(schedJSON),
		"jmx_xml": getString(sc, "jmx_xml"), "archived": 0,
		"updated_at": now, "created_at": now,
	})
	writer.insertAsync("load_scenarios", append(payload, '\n'))
}

func handlePerfScenarioSchedule(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if queryClient == nil || writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	var patch map[string]interface{}
	if json.Unmarshal(raw, &patch) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	sched := map[string]interface{}{}
	if s := getString(sc, "schedule_json"); s != "" && s != "{}" {
		_ = json.Unmarshal([]byte(s), &sched)
	}
	for k, v := range patch {
		sched[k] = v
	}
	if curve := parseCurveFromSchedule(sched); len(curve) > 0 {
		_, _, _ = applyLoadCurveToSchedule(curve, sched)
	}
	now := time.Now().UTC()
	if enabled, _ := sched["enabled"].(bool); enabled || getFloat64(sched, "enabled") == 1 {
		if getString(sched, "next_fire_at") == "" {
			sched["next_fire_at"] = nextFireFromSchedule(sched, now).Format(time.RFC3339)
		}
	}
	bumpScenarioSchedule(id, org, proj, sc, sched)
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "schedule": sched,
		"honesty": "Scheduler ticks in opl-api (every_minutes or daily_at HH:MM UTC). Lightweight in-process tick — not a distributed campaign scheduler.",
	})
}

// --- JTL import (offline analysis) ---

func handlePerfImportJTL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	scenarioID := strings.TrimSpace(r.URL.Query().Get("scenario_id"))
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if runID == "" {
		runID = loadID("run", org, proj, "jtl", fmt.Sprintf("%d", time.Now().UnixNano()))
	}

	var jtlBytes []byte
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "multipart error", 400)
			return
		}
		f, _, err := r.FormFile("jtl")
		if err != nil {
			f, _, err = r.FormFile("file")
		}
		if err != nil {
			http.Error(w, "jtl file required", 400)
			return
		}
		defer f.Close()
		jtlBytes, err = io.ReadAll(io.LimitReader(f, 32<<20))
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		if scenarioID == "" {
			scenarioID = strings.TrimSpace(r.FormValue("scenario_id"))
		}
	} else {
		var err error
		jtlBytes, err = io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil || len(jtlBytes) == 0 {
			http.Error(w, "jtl body required", 400)
			return
		}
	}

	tmp := filepath.Join(os.TempDir(), "opl-jtl-"+sanitizeDockerName(runID)+".jtl")
	if err := os.WriteFile(tmp, jtlBytes, 0o600); err != nil {
		http.Error(w, "temp write failed", 500)
		return
	}
	defer os.Remove(tmp)

	summary, samples := parseJTLFile(tmp)
	stampRunIDOnSamples(runID, org, proj, samples)
	if len(samples) > 2000 {
		samples = samples[:2000]
		summary["truncated"] = true
	}
	summary["imported"] = true
	summary["source"] = "jtl"
	status := "completed"
	if sc := loadScenarioMapForTenant(scenarioID, org, proj); sc != nil {
		sla := map[string]interface{}{}
		if s := getString(sc, "sla_json"); s != "" && s != "{}" {
			_ = json.Unmarshal([]byte(s), &sla)
		} else if s := getString(sc, "thresholds_json"); s != "" {
			_ = json.Unmarshal([]byte(s), &sla)
		}
		pass, reasons := evaluateSLAFailClosed(summary, sla)
		if !pass {
			status = "failed"
			summary["sla_reasons"] = reasons
		} else if len(sla) > 0 {
			status = "passed"
		}
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	sum, _ := json.Marshal(summary)
	payload, _ := json.Marshal(map[string]interface{}{
		"id": runID, "organization_id": org, "project_id": proj,
		"scenario_id": scenarioID, "status": status, "vus": 0,
		"started_at": now, "finished_at": now, "summary_json": string(sum), "error": "",
	})
	writer.insertAsync("load_runs", append(payload, '\n'))
	for _, s := range samples {
		samp, _ := json.Marshal(s)
		writer.insertAsync("load_run_samples", append(samp, '\n'))
	}
	if runStatusTerminal(status) {
		notifyRunTerminal(runNotifyEvent{
			RunID: runID, ScenarioID: scenarioID, OrganizationID: org, ProjectID: proj,
			Status: status, Summary: summary, FinishedAt: now, Source: "import-jtl",
		})
	}
	steps := aggregateRunSteps(samples)
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": runID, "load_run_id": runID, "status": status,
		"summary": summary, "steps": steps, "sample_count": len(samples),
		"honesty": "Imported JMeter JTL into load_runs/samples — offline analysis; not a live engine run.",
	})
}

// triageValidateResults adds pre-load triage hints for failed validate steps.
func triageValidateResults(results []map[string]interface{}) (pass bool, triage []map[string]interface{}) {
	pass = true
	for i, step := range results {
		ok, _ := step["ok"].(bool)
		if !ok {
			pass = false
		}
		typ, _ := step["type"].(string)
		needsTriage := !ok
		if typ == "extract" && !ok {
			needsTriage = true
		}
		if !needsTriage {
			continue
		}
		sev := classifyValidateFailure(step)
		entry := map[string]interface{}{
			"index": i, "type": typ, "name": step["name"], "ok": ok,
			"error": getString(step, "error"), "severity": sev, "hint": validateTriageHint(sev, step),
			"status_code": step["status_code"], "url": step["url"], "method": step["method"],
			"body_preview": step["body_preview"], "latency_ms": step["latency_ms"],
		}
		triage = append(triage, entry)
	}
	return pass, triage
}

func classifyValidateFailure(step map[string]interface{}) string {
	errMsg := strings.ToLower(getString(step, "error"))
	typ, _ := step["type"].(string)
	switch {
	case typ == "extract":
		return "extract_empty"
	case typ == "assert":
		return "assert_fail"
	case strings.Contains(errMsg, "url blocked"):
		return "url_policy"
	case strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline"):
		return "timeout"
	case strings.Contains(errMsg, "connection refused") || strings.Contains(errMsg, "no such host"):
		return "connectivity"
	case getFloat64(step, "status_code") >= 400:
		return "http_error"
	default:
		return "step_failed"
	}
}

func validateTriageHint(severity string, step map[string]interface{}) string {
	switch severity {
	case "url_policy":
		return "URL blocked by OPL SSRF policy — use compose service names or allowed hosts."
	case "timeout":
		return "Request timed out — check target reachability from opl-api."
	case "connectivity":
		return "Cannot reach host — on NAS prefer http://node-app:… on the compose network, not localhost."
	case "http_error":
		return fmt.Sprintf("HTTP %v — auth, path, or payload likely wrong before load.", step["status_code"])
	case "extract_empty":
		return "Empty extract — add/adjust extractor before load; dynamic tokens are the top replay failure."
	case "assert_fail":
		return "Assert failed on 1 VU — fix before dispatching workers."
	default:
		return "Step failed validation — inspect body_preview and vars before starting a load run."
	}
}

// suggestAutoCorrelation scans successful HTTP body previews for dynamic tokens
// and proposes extractors (jsonpath / regex) operators can apply under that step.
func suggestAutoCorrelation(results []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	seen := map[string]bool{}
	for i, step := range results {
		typ, _ := step["type"].(string)
		if typ != "" && typ != "http" {
			continue
		}
		ok, _ := step["ok"].(bool)
		body := fmt.Sprint(step["body_preview"])
		if body == "" || body == "<nil>" {
			continue
		}
		for _, sug := range detectCorrelationCandidates(body) {
			key := fmt.Sprintf("%d:%s:%s", i, sug["engine"], sug["expression"])
			if seen[key] {
				continue
			}
			seen[key] = true
			sug["step_index"] = i
			sug["step_name"] = step["name"]
			sug["apply_under"] = "http_children"
			if !ok {
				sug["note"] = "Step failed — suggestion still useful if body contains tokens."
			}
			out = append(out, sug)
			if len(out) >= 12 {
				return out
			}
		}
	}
	return out
}

var (
	reJSONToken  = regexp.MustCompile(`(?i)"(access_token|refresh_token|id_token|csrf[_-]?token|xsrf[_-]?token|session[_-]?id|auth[_-]?token|token|jwt)"\s*:\s*"([^"]{8,})"`)
	reHTMLHidden = regexp.MustCompile(`(?i)<input[^>]+name=["']([^"']*(?:csrf|token|nonce)[^"']*)["'][^>]+value=["']([^"']+)["']`)
	reHTMLHidden2 = regexp.MustCompile(`(?i)<input[^>]+value=["']([^"']+)["'][^>]+name=["']([^"']*(?:csrf|token|nonce)[^"']*)["']`)
	reBearer     = regexp.MustCompile(`(?i)Bearer\s+([A-Za-z0-9\-_\.=]{20,})`)
)

func detectCorrelationCandidates(body string) []map[string]interface{} {
	var out []map[string]interface{}
	add := func(engine, expr, vname, sample, why string) {
		out = append(out, map[string]interface{}{
			"engine": engine, "expression": expr, "var": vname,
			"sample": truncateStr(sample, 48), "reason": why,
			"type": "extract",
		})
	}
	for _, m := range reJSONToken.FindAllStringSubmatch(body, 6) {
		key := m[1]
		val := m[2]
		vname := sanitizeVarName(key)
		add("jsonpath", "$."+key, vname, val, "JSON field looks like a dynamic token")
	}
	for _, m := range reHTMLHidden.FindAllStringSubmatch(body, 4) {
		add("regex", `name=["']`+regexp.QuoteMeta(m[1])+`["'][^>]*value=["']([^"']+)["']`, sanitizeVarName(m[1]), m[2], "HTML hidden input (CSRF/token)")
	}
	for _, m := range reHTMLHidden2.FindAllStringSubmatch(body, 4) {
		add("regex", `value=["']([^"']+)["'][^>]*name=["']`+regexp.QuoteMeta(m[2])+`["']`, sanitizeVarName(m[2]), m[1], "HTML hidden input (CSRF/token)")
	}
	if m := reBearer.FindStringSubmatch(body); len(m) > 1 {
		add("regex", `Bearer\s+([A-Za-z0-9\-_\.=]+)`, "bearer_token", m[1], "Bearer token in response body")
	}
	return out
}

func sanitizeVarName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "token"
	}
	return string(out)
}

// --- Load policies (smooth/sustained/stress/custom → ramp/soak/spike) ---

type loadPolicyPreset struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Profile     string                 `json:"profile"`
	Description string                 `json:"description"`
	Defaults    map[string]interface{} `json:"defaults"`
	Honesty     string                 `json:"honesty"`
}

func perfLoadPolicyPresets() []loadPolicyPreset {
	return []loadPolicyPreset{
		{
			ID: "smooth", Label: "Smooth", Profile: "ramp",
			Description: "Ramp up, hold peak, ramp down.",
			Defaults:    map[string]interface{}{"ramp_up_pct": 30, "ramp_down_pct": 20, "hold_pct": 50},
			Honesty:     "Mapped to JMeter ThreadGroup ramp_time + duration on Docker workers — not multi-region geo injectors.",
		},
		{
			ID: "sustained", Label: "Sustained", Profile: "soak",
			Description: "Ramp to peak and hold (soak).",
			Defaults:    map[string]interface{}{"ramp_up_pct": 20, "ramp_down_pct": 0, "hold_pct": 80},
			Honesty:     "Long hold on one Docker host — not a multi-region soak grid.",
		},
		{
			ID: "stress", Label: "Stress", Profile: "spike",
			Description: "Short burst at high concurrency.",
			Defaults:    map[string]interface{}{"ramp_up_pct": 5, "ramp_down_pct": 5, "hold_pct": 90},
			Honesty:     "VU spike via workers×threads on local JMeter containers.",
		},
		{
			ID: "custom", Label: "Custom", Profile: "",
			Description: "Operator-supplied VUs, duration, ramp_seconds, workers, or schedule_json.curve.",
			Defaults:    map[string]interface{}{},
			Honesty:     "Custom schedule_json / workers / curve points only — ThreadGroup approximation, not ArrivalsThreadGroup.",
		},
	}
}

func handlePerfLoadPolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	writeJSON(w, map[string]interface{}{
		"policies": perfLoadPolicyPresets(),
		"honesty":  "OPL load policies map smooth→ramp, sustained→soak, stress→spike on Docker JMeter. Multi-cloud geo injectors are out of scope (use federation peers for peer fan-out only).",
	})
}

// resolveLoadPolicy maps policy id onto OPL profile + schedule hints.
func resolveLoadPolicy(policyID, profile string, schedule map[string]interface{}) (resolvedProfile string, sched map[string]interface{}, honesty string) {
	sched = map[string]interface{}{}
	for k, v := range schedule {
		sched[k] = v
	}
	id := strings.ToLower(strings.TrimSpace(policyID))
	if id == "" {
		id = strings.ToLower(strings.TrimSpace(profile))
	}
	for _, p := range perfLoadPolicyPresets() {
		if id == p.ID || id == strings.ToLower(p.Label) || (p.Profile != "" && id == p.Profile) {
			resolvedProfile = p.Profile
			if resolvedProfile == "" {
				resolvedProfile = profile
			}
			for k, v := range p.Defaults {
				if _, ok := sched[k]; !ok {
					sched[k] = v
				}
			}
			sched["policy"] = p.ID
			return resolvedProfile, sched, p.Honesty
		}
	}
	return profile, sched, "Unknown policy — using raw profile/schedule."
}

// aggregateRunSteps builds per-request/transaction stats from samples.
func aggregateRunSteps(samples []map[string]interface{}) []map[string]interface{} {
	type acc struct {
		n, okN     int
		sum        float64
		lats       []float64
		statusHits map[int]int
		url        string
	}
	by := map[string]*acc{}
	order := []string{}
	for _, s := range samples {
		label := strings.TrimSpace(getString(s, "step_name"))
		if label == "" {
			label = strings.TrimSpace(getString(s, "url"))
		}
		if label == "" {
			label = "(unnamed)"
		}
		a := by[label]
		if a == nil {
			a = &acc{statusHits: map[int]int{}, url: getString(s, "url")}
			by[label] = a
			order = append(order, label)
		}
		lat := getFloat64(s, "latency_ms")
		a.n++
		a.sum += lat
		a.lats = append(a.lats, lat)
		code := int(getFloat64(s, "status_code"))
		a.statusHits[code]++
		ok := false
		switch v := s["ok"].(type) {
		case bool:
			ok = v
		case float64:
			ok = v != 0
		case int:
			ok = v != 0
		case string:
			ok = v == "1" || strings.EqualFold(v, "true")
		}
		if ok || (code >= 200 && code < 400) {
			a.okN++
		}
		if a.url == "" {
			a.url = getString(s, "url")
		}
	}
	out := make([]map[string]interface{}, 0, len(order))
	for _, label := range order {
		a := by[label]
		sort.Float64s(a.lats)
		avg := 0.0
		if a.n > 0 {
			avg = a.sum / float64(a.n)
		}
		errRate := 0.0
		if a.n > 0 {
			errRate = float64(a.n-a.okN) / float64(a.n)
		}
		out = append(out, map[string]interface{}{
			"step_name":  label,
			"url":        a.url,
			"samples":    a.n,
			"errors":     a.n - a.okN,
			"error_rate": round4(errRate),
			"avg_ms":     round2(avg),
			"p50_ms":     round2(percentileSorted(a.lats, 0.50)),
			"p95_ms":     round2(percentileSorted(a.lats, 0.95)),
			"p99_ms":     round2(percentileSorted(a.lats, 0.99)),
			"min_ms":     round2(percentileSorted(a.lats, 0)),
			"max_ms":     round2(percentileSorted(a.lats, 1)),
		})
	}
	return out
}

func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func handlePerfRunSteps(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	samples := loadRunSampleMaps(r, runID, 5000)
	if samples == nil {
		http.Error(w, "not found", 404)
		return
	}
	steps := aggregateRunSteps(samples)
	writeJSON(w, map[string]interface{}{
		"run_id": runID, "steps": steps,
		"honesty": "Per-step stats from load_run_samples (JMeter labels) — request/transaction table, not a full bench report builder.",
	})
}

func handlePerfRunReport(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, vus, started_at, finished_at, summary_json, error
		FROM `+chTable("load_runs")+` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	run := rows[0]
	samples := loadRunSampleMaps(r, runID, 5000)
	if samples == nil {
		samples = []map[string]interface{}{}
	}
	steps := aggregateRunSteps(samples)
	summary := map[string]interface{}{}
	_ = json.Unmarshal([]byte(getString(run, "summary_json")), &summary)
	report := map[string]interface{}{
		"ok":           true,
		"run_id":       runID,
		"scenario_id":  getString(run, "scenario_id"),
		"status":       getString(run, "status"),
		"vus":          getFloat64(run, "vus"),
		"started_at":   getString(run, "started_at"),
		"finished_at":  getString(run, "finished_at"),
		"error":        getString(run, "error"),
		"summary":      summary,
		"steps":        steps,
		"sample_count": len(samples),
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"honesty":      "Structured bench report (JSON/CSV). PDF cover pages are out of scope.",
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"opl-report-%s.csv\"", runID))
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"step_name", "samples", "errors", "error_rate", "avg_ms", "p50_ms", "p95_ms", "p99_ms", "min_ms", "max_ms", "url"})
		for _, st := range steps {
			_ = cw.Write([]string{
				fmt.Sprint(st["step_name"]),
				fmt.Sprint(st["samples"]),
				fmt.Sprint(st["errors"]),
				fmt.Sprint(st["error_rate"]),
				fmt.Sprint(st["avg_ms"]),
				fmt.Sprint(st["p50_ms"]),
				fmt.Sprint(st["p95_ms"]),
				fmt.Sprint(st["p99_ms"]),
				fmt.Sprint(st["min_ms"]),
				fmt.Sprint(st["max_ms"]),
				fmt.Sprint(st["url"]),
			})
		}
		cw.Flush()
		return
	}
	writeJSON(w, report)
}

func handlePerfRunRunners(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, status, summary_json FROM `+chTable("load_runs")+` FINAL WHERE id = '%s'%s LIMIT 1`,
		escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	names := []string{}
	mode, image := "", ""
	workers := 0
	if st := lookupRunContainers(runID); st != nil {
		names = st.Containers
		mode, image, workers = st.Mode, st.Image, st.Workers
	}
	if len(names) == 0 {
		sum := map[string]interface{}{}
		_ = json.Unmarshal([]byte(getString(rows[0], "summary_json")), &sum)
		names = containerNamesFromAny(sum["containers"])
		if mode == "" {
			mode = getString(sum, "mode")
		}
		if image == "" {
			image = getString(sum, "image")
		}
		if workers == 0 {
			workers = int(getFloat64(sum, "workers"))
		}
	}
	// Predictable names when registry/summary empty but run is active (docker naming convention).
	if len(names) == 0 && !runStatusTerminal(getString(rows[0], "status")) {
		n := workers
		if n <= 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			names = append(names, fmt.Sprintf("opa-jmeter-%s-w%d", sanitizeDockerName(runID), i))
		}
		if mode == "" {
			mode = "docker"
		}
	}
	containers := make([]map[string]interface{}, 0, len(names))
	running := 0
	for _, name := range names {
		snap := dockerContainerSnapshot(name)
		containers = append(containers, snap)
		if ok, _ := snap["running"].(bool); ok {
			running++
		}
	}
	writeJSON(w, map[string]interface{}{
		"run_id": runID, "run_status": getString(rows[0], "status"),
		"mode": mode, "image": image, "workers": workers,
		"running": running, "containers": containers,
		"honesty": "Live docker inspect of ephemeral JMeter workers on the local host only (≠ multi-region geo locations).",
	})
}

func loadRunSampleMaps(r *http.Request, runID string, limit int) []map[string]interface{} {
	if queryClient == nil || runID == "" {
		return nil
	}
	owned, err := queryClient.Query(fmt.Sprintf(`
		SELECT id FROM `+chTable("load_runs")+` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(owned) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 2000
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT run_id, ts, latency_ms, status_code, ok, url, step_name
		FROM `+chTable("load_run_samples")+` WHERE run_id = '%s'%s
		ORDER BY ts ASC LIMIT %d`, escapeSQL(runID), perfOwnedAnd(r), limit))
	if err != nil {
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT run_id, ts, latency_ms, status_code, ok, url
			FROM `+chTable("load_run_samples")+` WHERE run_id = '%s'%s
			ORDER BY ts ASC LIMIT %d`, escapeSQL(runID), perfOwnedAnd(r), limit))
		if err != nil {
			return []map[string]interface{}{}
		}
	}
	return rows
}

// --- Scenario archive / duplicate ---

func scenarioArchivedAnd() string {
	return " AND coalesce(archived, 0) = 0"
}

func handlePerfScenarioArchive(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if queryClient == nil || writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": getString(sc, "name"), "target_url": getString(sc, "target_url"),
		"method": nz(getString(sc, "method"), "GET"),
		"vus": int(getFloat64(sc, "vus")), "duration_seconds": int(getFloat64(sc, "duration_seconds")),
		"headers_json": nz(getString(sc, "headers_json"), "{}"), "body": getString(sc, "body"),
		"thresholds_json": nz(getString(sc, "thresholds_json"), "{}"),
		"steps_json": nz(getString(sc, "steps_json"), "[]"), "datasets_json": nz(getString(sc, "datasets_json"), "{}"),
		"sla_json": nz(getString(sc, "sla_json"), "{}"), "schedule_json": nz(getString(sc, "schedule_json"), "{}"),
		"jmx_xml": getString(sc, "jmx_xml"), "archived": 1,
		"updated_at": now, "created_at": now,
	})
	writer.insertAsync("load_scenarios", append(payload, '\n'))
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "archived": true,
		"honesty": "Soft-archive via ReplacingMergeTree (archived=1). POST .../unarchive to restore.",
	})
}

func handlePerfScenarioUnarchive(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if queryClient == nil || writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sc := loadScenarioMapReqAny(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": getString(sc, "name"), "target_url": getString(sc, "target_url"),
		"method": nz(getString(sc, "method"), "GET"),
		"vus": int(getFloat64(sc, "vus")), "duration_seconds": int(getFloat64(sc, "duration_seconds")),
		"headers_json": nz(getString(sc, "headers_json"), "{}"), "body": getString(sc, "body"),
		"thresholds_json": nz(getString(sc, "thresholds_json"), "{}"),
		"steps_json": nz(getString(sc, "steps_json"), "[]"), "datasets_json": nz(getString(sc, "datasets_json"), "{}"),
		"sla_json": nz(getString(sc, "sla_json"), "{}"), "schedule_json": nz(getString(sc, "schedule_json"), "{}"),
		"jmx_xml": getString(sc, "jmx_xml"), "archived": 0,
		"updated_at": now, "created_at": now,
	})
	writer.insertAsync("load_scenarios", append(payload, '\n'))
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "archived": false,
		"honesty": "Restored soft-archived scenario (archived=0 via ReplacingMergeTree).",
	})
}

func handlePerfScenarioDuplicate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if queryClient == nil || writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	name := getString(sc, "name")
	if name == "" {
		name = id
	}
	newName := name + " (copy)"
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var body struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &body) == nil && strings.TrimSpace(body.Name) != "" {
			newName = strings.TrimSpace(body.Name)
		}
	}
	newID := loadID("scn", org, proj, newName, fmt.Sprintf("%d", time.Now().UnixNano()))
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": newID, "organization_id": org, "project_id": proj,
		"name": newName, "target_url": getString(sc, "target_url"),
		"method": nz(getString(sc, "method"), "GET"),
		"vus": int(getFloat64(sc, "vus")), "duration_seconds": int(getFloat64(sc, "duration_seconds")),
		"headers_json": nz(getString(sc, "headers_json"), "{}"), "body": getString(sc, "body"),
		"thresholds_json": nz(getString(sc, "thresholds_json"), "{}"),
		"steps_json": nz(getString(sc, "steps_json"), "[]"), "datasets_json": nz(getString(sc, "datasets_json"), "{}"),
		"sla_json": nz(getString(sc, "sla_json"), "{}"), "schedule_json": nz(getString(sc, "schedule_json"), "{}"),
		"jmx_xml": getString(sc, "jmx_xml"), "archived": 0,
		"updated_at": now, "created_at": now,
	})
	writer.insertAsync("load_scenarios", append(payload, '\n'))
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": newID, "name": newName, "source_id": id,
		"honesty": "Duplicated scenario — JMX/steps copied into a new id.",
	})
}
