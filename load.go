package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Perf lab — load scenarios, runs, metrics, k6 export (single-runner MVP).

func registerPerfLabMux(mux *http.ServeMux, authView, authAdmin func(string, http.HandlerFunc)) {
	authView("/api/perf/scenarios", handlePerfScenarios)
	authView("/api/perf/runs", handlePerfRunsListOrCreate)
	authView("/api/perf/runs/", handlePerfRunByID)
	authAdmin("/api/perf/scenarios/upsert", handlePerfScenarioUpsert)
	_ = mux
}

func loadID(prefix string, parts ...string) string {
	h := sha1.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return prefix + "-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func handlePerfScenarios(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		http.Error(w, "use POST /api/perf/scenarios/upsert (admin)", 405)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		writeJSON(w, map[string]interface{}{"scenarios": []interface{}{}})
		return
	}
	scope := tenantScopeSQL(r, queryClient, "")
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, target_url, method, vus, duration_seconds, headers_json, thresholds_json,
			steps_json, datasets_json, sla_json, schedule_json,
			length(jmx_xml) AS jmx_bytes, updated_at
		FROM ` + chTable("load_scenarios") + ` FINAL WHERE 1=1%s
		ORDER BY updated_at DESC LIMIT 100`, scope))
	if err != nil {
		// Pre-migration 0032 fallback.
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT id, name, target_url, method, vus, duration_seconds, headers_json, thresholds_json, updated_at
			FROM ` + chTable("load_scenarios") + ` FINAL WHERE 1=1%s
			ORDER BY updated_at DESC LIMIT 100`, scope))
		if err != nil {
			writeJSON(w, map[string]interface{}{"scenarios": []interface{}{}})
			return
		}
	}
	writeJSON(w, map[string]interface{}{
		"scenarios": rows,
		"honesty":   "Docker JMeter engine (default) — federation fan-out ≠ multi-region load cloud.",
		"engine":    strings.ToLower(envOr("OPA_PERF_ENGINE", "jmeter")),
		"runner":    "docker",
	})
}

// loadRunIDFromHeaders extracts X-OPA-Load-Run-Id / baggage from request header maps
// when instrumented apps record them onto spans (cheap ingest-side helper).
func loadRunIDFromHeaders(headers map[string]interface{}) string {
	if headers == nil {
		return ""
	}
	for _, k := range []string{"X-OPA-Load-Run-Id", "x-opa-load-run-id", "X-Opa-Load-Run-Id"} {
		if v, ok := headers[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if bag, ok := headers["baggage"].(string); ok {
		for _, part := range strings.Split(bag, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "opa.load_run_id=") {
				return strings.TrimPrefix(part, "opa.load_run_id=")
			}
			if strings.HasPrefix(part, "load_run_id=") {
				return strings.TrimPrefix(part, "load_run_id=")
			}
		}
	}
	return ""
}

func handlePerfScenarioUpsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	var body struct {
		ID              string          `json:"id"`
		Name            string          `json:"name"`
		TargetURL       string          `json:"target_url"`
		Method          string          `json:"method"`
		VUs             int             `json:"vus"`
		DurationSeconds int             `json:"duration_seconds"`
		Headers         json.RawMessage `json:"headers"`
		Body            string          `json:"body"`
		Thresholds      json.RawMessage `json:"thresholds"`
		Steps           json.RawMessage `json:"steps"`
		Datasets        json.RawMessage `json:"datasets"`
		SLA             json.RawMessage `json:"sla"`
		Schedule        json.RawMessage `json:"schedule"`
		JMXXML          string          `json:"jmx_xml"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Name == "" {
		http.Error(w, "name required", 400)
		return
	}
	if body.VUs <= 0 {
		body.VUs = 10
	}
	body.VUs = clampPerfVUs(body.VUs)
	if body.DurationSeconds <= 0 {
		body.DurationSeconds = 60
	}
	body.DurationSeconds = clampPerfDuration(body.DurationSeconds)
	if body.TargetURL == "" {
		body.TargetURL = "http://127.0.0.1:8080/api/health"
	}
	id := body.ID
	if id == "" {
		id = loadID("scn", org, proj, body.Name, fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	headers := body.Headers
	if len(headers) == 0 {
		headers = json.RawMessage(`{}`)
	}
	thresh := body.Thresholds
	if len(thresh) == 0 {
		thresh = json.RawMessage(`{"p95_ms":500,"error_rate_max":0.05}`)
	}
	steps := body.Steps
	if len(steps) == 0 {
		steps = json.RawMessage(`[]`)
	}
	datasets := body.Datasets
	if len(datasets) == 0 {
		datasets = json.RawMessage(`{}`)
	}
	sla := body.SLA
	if len(sla) == 0 {
		sla = thresh
	}
	schedule := body.Schedule
	if len(schedule) == 0 {
		schedule = json.RawMessage(`{}`)
	}
	jmx := body.JMXXML
	if strings.TrimSpace(jmx) == "" {
		jmx = generateJMXFromUpsert(body.Name, body.TargetURL, nz(body.Method, "GET"), body.Body, body.VUs, body.DurationSeconds, steps)
	}
	if jmxContainsUnsafeElements(jmx) {
		http.Error(w, "jmx_xml contains unsafe JMeter elements (script/OS samplers); set OPA_PERF_ALLOW_UNSAFE_JMX=1 to override", 400)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj, "name": body.Name,
		"target_url": body.TargetURL, "method": nz(body.Method, "GET"),
		"vus": body.VUs, "duration_seconds": body.DurationSeconds,
		"headers_json": string(headers), "body": body.Body, "thresholds_json": string(thresh),
		"steps_json": string(steps), "datasets_json": string(datasets),
		"sla_json": string(sla), "schedule_json": string(schedule), "jmx_xml": jmx,
		"updated_at": now, "created_at": now,
	})
	if writer != nil {
		writer.insertAsync("load_scenarios", append(payload, '\n'))
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id,
		"honesty": "JMeter-compatible scenario; jmx_xml is source of truth for Docker JMeter runs.",
	})
}

func handlePerfRunsListOrCreate(w http.ResponseWriter, r *http.Request) {
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			ScenarioID string          `json:"scenario_id"`
			VUs        int             `json:"vus"`
			Fanout     bool            `json:"fanout"`
			Profile    string          `json:"profile"` // soak|spike|ramp|""
			Dispatch   bool            `json:"dispatch"`
			Engine     string          `json:"engine"` // jmeter|node
			Workers    int             `json:"workers"` // JMeter container fan-out (VU scale)
			Schedule   json.RawMessage `json:"schedule"`
		}
		_ = json.Unmarshal(raw, &body)
		wantDispatch := body.Dispatch || strings.EqualFold(envOr("OPA_PERF_AUTO_DISPATCH", ""), "1")
		if (wantDispatch || body.Fanout) && !perfRequireAdmin(w, r) {
			return
		}
		// Tenancy: scenario must be owned by caller when auth is on.
		if body.ScenarioID != "" {
			if sc := loadScenarioMapReq(r, body.ScenarioID); sc == nil {
				http.Error(w, "scenario not found", 404)
				return
			}
		}
		id := loadID("run", org, proj, body.ScenarioID, fmt.Sprintf("%d", time.Now().UnixNano()))
		now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
		vus := body.VUs
		if vus <= 0 {
			vus = 10
		}
		if body.Profile == "soak" && vus < 5 {
			vus = 5
		}
		if body.Profile == "spike" && vus < 50 {
			vus = 50
		}
		vus = clampPerfVUs(vus)
		peerResults := []map[string]interface{}{}
		fanoutHonesty := "Docker JMeter containers by default; federation fan-out ≠ multi-region load cloud."
		if body.Fanout {
			peerResults = fanoutLoadToPeers(body.ScenarioID, id, vus, org, proj)
			remotePeers := 0
			for _, p := range federationPeersSnapshot() {
				if p.Enabled && p.BaseURL != "" && !strings.EqualFold(p.Region, localAgentRegion()) {
					remotePeers++
				}
			}
			if remotePeers == 0 {
				fanoutHonesty = "Fan-out local-sample-only — no federation peers configured (set OPA_FEDERATION_PEERS or enable opa.federation_peers). ≠ multi-region load cloud."
			}
		}
		engine := strings.ToLower(nz(body.Engine, envOr("OPA_PERF_ENGINE", "jmeter")))
		dispatchInfo := map[string]interface{}{"dispatched": false}
		provisional := "created"
		if wantDispatch {
			provisional = "running"
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"id": id, "organization_id": org, "project_id": proj,
			"scenario_id": body.ScenarioID, "status": provisional, "vus": vus,
			"started_at": now, "finished_at": now, "summary_json": "{}", "error": "",
		})
		if writer != nil {
			writer.insertAsync("load_runs", append(payload, '\n'))
		}
		if wantDispatch {
			if engine == "jmeter" || engine == "" {
				dispatchInfo = dispatchJMeterRunScaled(body.ScenarioID, id, vus, body.Workers, org, proj)
				if ok, _ := dispatchInfo["dispatched"].(bool); !ok && nodePerfFallbackAllowed() {
					dispatchInfo["node_fallback"] = maybeDispatchLoadRunner(body.ScenarioID, id, vus, body.Profile, org, proj)
				}
			} else if engine == "node" {
				if !nodePerfFallbackAllowed() {
					dispatchInfo = map[string]interface{}{
						"dispatched": false,
						"error":      "Node engine is dev-only; set OPA_PERF_ALLOW_NODE_FALLBACK=1 (production path is Docker JMeter)",
					}
				} else {
					dispatchInfo = maybeDispatchLoadRunner(body.ScenarioID, id, vus, body.Profile, org, proj)
				}
			} else {
				dispatchInfo = map[string]interface{}{"dispatched": false, "error": "unknown engine: " + engine}
			}
		} else {
			dispatchInfo["tip"] = "Pass dispatch:true (admin) to spawn ephemeral JMeter Docker container(s)."
		}
		runStatus, runErr := initialLoadRunStatus(wantDispatch, dispatchInfo)
		if runStatus != provisional && writer != nil {
			summaryObj := map[string]interface{}{}
			if runErr != "" {
				summaryObj["dispatch_error"] = runErr
			}
			sumBytes, _ := json.Marshal(summaryObj)
			if len(sumBytes) == 0 {
				sumBytes = []byte("{}")
			}
			fix := time.Now().UTC().Format("2006-01-02 15:04:05.000")
			fixPayload, _ := json.Marshal(map[string]interface{}{
				"id": id, "organization_id": org, "project_id": proj,
				"scenario_id": body.ScenarioID, "status": runStatus, "vus": vus,
				"started_at": now, "finished_at": fix, "summary_json": string(sumBytes), "error": runErr,
			})
			writer.insertAsync("load_runs", append(fixPayload, '\n'))
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "id": id, "load_run_id": id, "status": runStatus, "profile": body.Profile, "engine": engine,
			"headers": map[string]string{
				"X-OPA-Load-Run-Id": id,
				"baggage":           "load_run_id=" + id,
			},
			"fanout_peers": peerResults,
			"dispatch":     dispatchInfo,
			"honesty":      fanoutHonesty,
		})
		return
	}
	if queryClient == nil {
		writeJSON(w, map[string]interface{}{"runs": []interface{}{}})
		return
	}
	scope := tenantScopeSQL(r, queryClient, "")
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, vus, started_at, finished_at, summary_json, error
		FROM ` + chTable("load_runs") + ` FINAL WHERE 1=1%s
		ORDER BY started_at DESC LIMIT 100`, scope))
	if err != nil {
		writeJSON(w, map[string]interface{}{"runs": []interface{}{}})
		return
	}
	writeJSON(w, map[string]interface{}{"runs": rows})
}

func handlePerfRunByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/perf/runs/")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if id == "" {
		http.Error(w, "id required", 400)
		return
	}
	if len(parts) > 1 && parts[1] == "export-k6" {
		handlePerfExportK6(w, r, id)
		return
	}
	if len(parts) > 1 && parts[1] == "metrics" {
		handlePerfRunMetrics(w, r, id)
		return
	}
	if len(parts) > 1 && parts[1] == "samples" {
		handlePerfRunSamples(w, r, id)
		return
	}
	if len(parts) > 1 && parts[1] == "gate" {
		handlePerfRunGate(w, r, id)
		return
	}
	if len(parts) > 1 && parts[1] == "cancel" {
		handlePerfRunCancel(w, r, id)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, vus, started_at, finished_at, summary_json, error
		FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, rows[0])
}

func handlePerfRunCancel(w http.ResponseWriter, r *http.Request, id string) {
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
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, vus, started_at, summary_json, error
		FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	cur := getString(rows[0], "status")
	if runStatusTerminal(cur) || strings.EqualFold(cur, "created") {
		writeJSON(w, map[string]interface{}{
			"ok": true, "id": id, "status": cur, "honesty": "already terminal — no cancel needed",
		})
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	started := getString(rows[0], "started_at")
	if started == "" {
		started = now
	}
	summary := getString(rows[0], "summary_json")
	if summary == "" {
		summary = "{}"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"scenario_id": getString(rows[0], "scenario_id"), "status": "cancelled",
		"vus": int(getFloat64(rows[0], "vus")), "started_at": started, "finished_at": now,
		"summary_json": summary, "error": "cancelled by user",
	})
	writer.insertAsync("load_runs", append(payload, '\n'))
	writeJSON(w, map[string]interface{}{"ok": true, "id": id, "status": "cancelled"})
}

func handlePerfExportK6(w http.ResponseWriter, r *http.Request, scenarioOrRunID string) {
	scenarioID := r.URL.Query().Get("scenario_id")
	if scenarioID == "" {
		scenarioID = scenarioOrRunID
	}
	target, method := "http://127.0.0.1:8080/api/health", "GET"
	vus, dur := 10, 60
	if sc := loadScenarioMapReq(r, scenarioID); sc != nil {
		if s := getString(sc, "target_url"); s != "" {
			target = s
		}
		if s := getString(sc, "method"); s != "" {
			method = s
		}
		if v := int(getFloat64(sc, "vus")); v > 0 {
			vus = v
		}
		if d := int(getFloat64(sc, "duration_seconds")); d > 0 {
			dur = d
		}
	}
	script := fmt.Sprintf(`import http from 'k6/http';
import { sleep, check } from 'k6';
export const options = { vus: %d, duration: '%ds', tags: { load_run_id: '%s' } };
const loadRunId = __ENV.OPA_LOAD_RUN_ID || '%s';
export default function () {
  const res = http.%s(%q, { headers: {
    'traceparent': '00-' + (Math.random().toString(16).slice(2).padEnd(32,'0').slice(0,32)) + '-0000000000000001-01',
    'X-OPA-Load-Run-Id': loadRunId,
    'baggage': 'opa.load_run_id=' + loadRunId,
  }});
  check(res, { 'status < 500': (r) => r.status < 500 });
  sleep(0.1);
}
`, vus, dur, scenarioOrRunID, scenarioOrRunID, strings.ToLower(method), target)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

func handlePerfRunMetrics(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfAllowMetricsWrite(r) {
		http.Error(w, "admin or runner token required", 403)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	if !enforceWriteLocalityHTTP(w, r, org, proj) {
		return
	}
	runScenarioID := ""
	if queryClient != nil {
		rows, err := queryClient.Query(fmt.Sprintf(`
			SELECT id, scenario_id FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), perfOwnedAnd(r)))
		if err != nil || len(rows) == 0 {
			http.Error(w, "run not found", 404)
			return
		}
		runScenarioID = getString(rows[0], "scenario_id")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	var body struct {
		ScenarioID string                 `json:"scenario_id"`
		Status     string                 `json:"status"`
		VUs        int                    `json:"vus"`
		Summary    map[string]interface{} `json:"summary"`
		Error      string                 `json:"error"`
		Samples    []struct {
			LatencyMs  float64 `json:"latency_ms"`
			StatusCode int     `json:"status_code"`
			OK         bool    `json:"ok"`
			URL        string  `json:"url"`
		} `json:"samples"`
	}
	if json.Unmarshal(raw, &body) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	scenarioID := runScenarioID
	if scenarioID == "" {
		scenarioID = body.ScenarioID
	}
	clientStatus := nz(body.Status, "completed")
	status := clientStatus
	errMsg := body.Error
	summary := body.Summary
	if summary == nil {
		summary = map[string]interface{}{}
	}
	// Ignore client pass/fail when evaluating against scenario SLA/thresholds.
	if !strings.EqualFold(clientStatus, "running") {
		sla := map[string]interface{}{}
		sc := loadScenarioMapForTenant(scenarioID, org, proj)
		if sc == nil {
			sc = loadScenarioMapReq(r, scenarioID)
		}
		if sc != nil {
			if s := getString(sc, "sla_json"); s != "" && s != "{}" {
				_ = json.Unmarshal([]byte(s), &sla)
			} else if s := getString(sc, "thresholds_json"); s != "" {
				_ = json.Unmarshal([]byte(s), &sla)
			}
		}
		pass, reasons := evaluateSLAFailClosed(summary, sla)
		if pass {
			if len(sla) > 0 {
				status = "passed"
			} else {
				status = "completed"
			}
		} else {
			status = "failed"
			summary["sla_reasons"] = reasons
			if errMsg == "" {
				errMsg = strings.Join(reasons, "; ")
			}
		}
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	sum, _ := json.Marshal(summary)
	if len(sum) == 0 {
		sum = []byte("{}")
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"scenario_id": scenarioID, "status": status,
		"vus": body.VUs, "started_at": now, "finished_at": now,
		"summary_json": string(sum), "error": errMsg,
	})
	if writer != nil {
		writer.insertAsync("load_runs", append(payload, '\n'))
		for _, s := range body.Samples {
			ok := 0
			if s.OK {
				ok = 1
			}
			samp, _ := json.Marshal(map[string]interface{}{
				"run_id": id, "organization_id": org, "project_id": proj,
				"ts": now, "latency_ms": s.LatencyMs, "status_code": s.StatusCode,
				"ok": ok, "url": s.URL,
			})
			writer.insertAsync("load_run_samples", append(samp, '\n'))
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "id": id, "status": status})
}

// fanoutLoadToPeers asks federation peers to run remote-load and returns peer metrics.
// Honesty: peer fan-out ≠ multi-cloud commercial load grid.
func fanoutLoadToPeers(scenarioID, runID string, vus int, org, proj string) []map[string]interface{} {
	out := []map[string]interface{}{}
	token := strings.TrimSpace(envOr("OPA_FEDERATION_TOKEN", ""))
	target, method, dur := "http://127.0.0.1:8080/api/health", "GET", 15
	if sc := loadScenarioMapForTenant(scenarioID, org, proj); sc != nil {
		if s := getString(sc, "target_url"); s != "" {
			target = s
		}
		if s := getString(sc, "method"); s != "" {
			method = s
		}
		if d := int(getFloat64(sc, "duration_seconds")); d > 0 {
			dur = d
			if dur > 60 {
				dur = 60
			}
		}
		if err := perfScenarioHTTPURLsBlocked(sc); err != nil {
			return []map[string]interface{}{{
				"peer_id": "local", "ok": false, "error": "url policy: " + err.Error(),
			}}
		}
	} else if scenarioID != "" {
		return []map[string]interface{}{{
			"peer_id": "local", "ok": false, "error": "scenario not found for tenant",
		}}
	}
	// Local sample always included so fan-out is useful with zero peers.
	local := map[string]interface{}{"peer_id": "local", "region": localAgentRegion()}
	if err := isBlockedPerfURL(target); err != nil {
		local["ok"] = false
		local["error"] = "url blocked: " + err.Error()
	} else {
		local = runLocalLoadSample(target, method, vus, dur, runID, false)
		local["peer_id"] = "local"
		local["region"] = localAgentRegion()
		local["ok"] = true
	}
	out = append(out, local)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, peer := range federationPeersSnapshot() {
		if !peer.Enabled || peer.BaseURL == "" {
			continue
		}
		if strings.EqualFold(peer.Region, localAgentRegion()) {
			continue
		}
		wg.Add(1)
		go func(peer federationPeer) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]interface{}{
				"scenario_id": scenarioID, "load_run_id": runID, "vus": vus,
				"target_url": target, "method": method, "duration_seconds": dur,
				"organization_id": org, "project_id": proj,
				"engine": "jmeter",
				"honesty": "Peer runs the same scenario (JMeter when available) — not a multi-region load cloud.",
			})
			req, err := http.NewRequest(http.MethodPost, strings.TrimRight(peer.BaseURL, "/")+"/api/federation/remote-load", strings.NewReader(string(body)))
			entry := map[string]interface{}{"peer_id": peer.ID, "region": peer.Region}
			if err != nil {
				entry["ok"] = false
				entry["error"] = err.Error()
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("X-OPA-Federation-Token", token)
				req.Header.Set("Authorization", "Bearer "+token)
			}
			start := time.Now()
			resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
			lat := float64(time.Since(start).Milliseconds())
			entry["dispatch_ms"] = lat
			if err != nil {
				entry["ok"] = false
				entry["error"] = err.Error()
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			entry["status"] = resp.StatusCode
			if resp.StatusCode < 300 {
				entry["ok"] = true
				var m map[string]interface{}
				_ = json.Unmarshal(raw, &m)
				for k, v := range m {
					entry[k] = v
				}
			} else {
				entry["ok"] = false
				entry["error"] = truncateStr(string(raw), 120)
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}(peer)
	}
	wg.Wait()
	return out
}
