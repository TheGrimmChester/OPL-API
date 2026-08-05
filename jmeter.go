package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// JMeter Perf Lab: JMeter-shaped Perf Lab — multi-step scenarios, JMX import/export,
// validate dry-run, SLA gate. Production execution is ephemeral Docker JMeter
// containers (PerfContainerRunner); Node load-runner is a gated dev fallback.

func registerJMeterMux(mux *http.ServeMux, authView, authAdmin func(string, http.HandlerFunc)) {
	authAdmin("/api/perf/scenarios/import-jmx", handlePerfImportJMX)
	authAdmin("/api/perf/runs/import-jtl", handlePerfImportJTL)
	registerHARCaptureMux(mux, authView, authAdmin)
	registerPostmanMux(mux, authView, authAdmin)
	authView("/api/perf/scenarios/", handlePerfScenarioSubroutes)
	_ = mux
}

func handlePerfScenarioSubroutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/perf/scenarios/")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "id required", 400)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			handlePerfScenarioArchive(w, r, id)
			return
		}
		handlePerfScenarioGet(w, r, id)
		return
	}
	switch parts[1] {
	case "validate":
		if !perfRequireAdmin(w, r) {
			return
		}
		handlePerfScenarioValidate(w, r, id)
	case "export-jmx":
		handlePerfExportJMX(w, r, id)
	case "export-xhr":
		handlePerfExportXHR(w, r, id)
	case "export-har":
		handlePerfExportHAR(w, r, id)
	case "gate":
		handlePerfScenarioGate(w, r, id)
	case "archive":
		handlePerfScenarioArchive(w, r, id)
	case "unarchive":
		handlePerfScenarioUnarchive(w, r, id)
	case "duplicate":
		handlePerfScenarioDuplicate(w, r, id)
	case "schedule":
		sub := ""
		if len(parts) > 2 {
			sub = parts[2]
		}
		handlePerfScenarioSchedule(w, r, id, sub)
	case "trends":
		handlePerfScenarioTrends(w, r, id)
	default:
		http.Error(w, "not found", 404)
	}
}

func handlePerfScenarioGet(w http.ResponseWriter, r *http.Request, id string) {
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	owned := perfOwnedAnd(r)
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, target_url, method, vus, duration_seconds,
			headers_json, body, thresholds_json, steps_json, datasets_json, sla_json, schedule_json, jmx_xml, updated_at
		FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s%s LIMIT 1`, escapeSQL(id), owned, scenarioArchivedAnd()))
	if err != nil || len(rows) == 0 {
		// Fallback without archived / JMeter columns for pre-migration agents.
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body, thresholds_json, updated_at
			FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), owned))
		if err != nil || len(rows) == 0 {
			http.Error(w, "not found", 404)
			return
		}
	}
	writeJSON(w, rows[0])
}

// --- JMX import (best-effort) ---

type jmxTestPlan struct {
	XMLName xml.Name `xml:"jmeterTestPlan"`
	HashTree []jmxHashTree `xml:"hashTree"`
}

type jmxHashTree struct {
	ThreadGroups []jmxThreadGroup `xml:"ThreadGroup"`
	HTTPSamplers []jmxHTTPSampler `xml:"HTTPSamplerProxy"`
	RegexExtract []jmxRegexExtractor `xml:"RegexExtractor"`
	CSVDataSets  []jmxCSVDataSet `xml:"CSVDataSet"`
	ConstantTimers []jmxConstantTimer `xml:"ConstantTimer"`
	HashTrees    []jmxHashTree `xml:"hashTree"`
	// Also catch elements nested under first hashTree children in any order via recursive walk.
}

type jmxThreadGroup struct {
	Enabled string `xml:"enabled,attr"`
	Props   []jmxProp `xml:"stringProp"`
	Bools   []jmxBoolProp `xml:"boolProp"`
	Ints    []jmxIntProp `xml:"intProp"`
	Longs   []jmxLongProp `xml:"longProp"`
	Elements []jmxElementProp `xml:"elementProp"`
}

type jmxHTTPSampler struct {
	TestName string `xml:"testname,attr"`
	Enabled  string `xml:"enabled,attr"`
	Props    []jmxProp `xml:"stringProp"`
	Bools    []jmxBoolProp `xml:"boolProp"`
}

type jmxRegexExtractor struct {
	TestName string `xml:"testname,attr"`
	Props    []jmxProp `xml:"stringProp"`
}

type jmxCSVDataSet struct {
	TestName string `xml:"testname,attr"`
	Props    []jmxProp `xml:"stringProp"`
	Bools    []jmxBoolProp `xml:"boolProp"`
}

type jmxConstantTimer struct {
	TestName string `xml:"testname,attr"`
	Props    []jmxProp `xml:"stringProp"`
}

type jmxProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type jmxBoolProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type jmxIntProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type jmxLongProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type jmxElementProp struct {
	Name  string `xml:"name,attr"`
	Props []jmxProp `xml:"stringProp"`
}

func jmxPropMap(props []jmxProp) map[string]string {
	m := map[string]string{}
	for _, p := range props {
		m[p.Name] = strings.TrimSpace(p.Value)
	}
	return m
}

func walkJMXTree(ht jmxHashTree, tgs *[]jmxThreadGroup, https *[]jmxHTTPSampler, regs *[]jmxRegexExtractor, csvs *[]jmxCSVDataSet, timers *[]jmxConstantTimer) {
	*tgs = append(*tgs, ht.ThreadGroups...)
	*https = append(*https, ht.HTTPSamplers...)
	*regs = append(*regs, ht.RegexExtract...)
	*csvs = append(*csvs, ht.CSVDataSets...)
	*timers = append(*timers, ht.ConstantTimers...)
	for _, child := range ht.HashTrees {
		walkJMXTree(child, tgs, https, regs, csvs, timers)
	}
}

func parseJMXToScenario(raw []byte, name string) (map[string]interface{}, []string, error) {
	warnings := []string{}
	if strings.Contains(string(raw), "kg.apc.jmeter") || strings.Contains(string(raw), "com.blazemeter") {
		warnings = append(warnings, "Third-party JMeter plugins detected — treated as opaque; not imported.")
	}

	// Prefer nested tree parse (If/While/Loop/Transaction + nested extractors).
	// Test Plan level fragments come back first so the module references inside the
	// thread group still resolve against them after the round trip.
	if treeSteps := extractStepsFromJMXTree(raw); len(treeSteps) > 0 {
		if frags := extractFragmentStepsFromJMX(raw); len(frags) > 0 {
			treeSteps = append(frags, treeSteps...)
		}
		vus, dur, ramp := 10, 60, 0
		var plan jmxTestPlan
		if xml.Unmarshal(raw, &plan) == nil {
			var tgs []jmxThreadGroup
			var https []jmxHTTPSampler
			var regs []jmxRegexExtractor
			var csvs []jmxCSVDataSet
			var timers []jmxConstantTimer
			for _, ht := range plan.HashTree {
				walkJMXTree(ht, &tgs, &https, &regs, &csvs, &timers)
			}
			if len(tgs) > 0 {
				pm := jmxPropMap(tgs[0].Props)
				if n := pm["ThreadGroup.num_threads"]; n != "" {
					fmt.Sscanf(n, "%d", &vus)
				}
				if n := pm["ThreadGroup.ramp_time"]; n != "" {
					fmt.Sscanf(n, "%d", &ramp)
				}
				if n := pm["ThreadGroup.duration"]; n != "" {
					fmt.Sscanf(n, "%d", &dur)
				}
			}
			_ = https
			_ = regs
			_ = timers
			datasets := map[string]interface{}{}
			for _, c := range csvs {
				datasets["csv"] = perfDatasetFromJMXCSVDataSet(c)
			}
			if dur <= 0 {
				dur = 60
			}
			schedule := map[string]interface{}{}
			if ramp > 0 {
				schedule["ramp_seconds"] = ramp
				schedule["profile"] = "ramp"
			}
			firstURL, firstMethod := firstHTTPFromSteps(treeSteps)
			return map[string]interface{}{
				"name": nz(name, "imported-jmx"), "target_url": firstURL, "method": firstMethod,
				"vus": vus, "duration_seconds": dur, "steps": treeSteps,
				"datasets": datasets, "schedule": schedule,
				"sla":     map[string]interface{}{"p95_ms": 500, "error_rate_max": 0.05},
				"honesty": "JMX import preserves nested If/While/Loop/Transaction controllers when present; HTTP samplers, extractors, CSV, classic thread groups.",
			}, warnings, nil
		}
		firstURL, firstMethod := firstHTTPFromSteps(treeSteps)
		return map[string]interface{}{
			"name": nz(name, "imported-jmx"), "target_url": firstURL, "method": firstMethod,
			"vus": 10, "duration_seconds": 60, "steps": treeSteps,
			"datasets": map[string]interface{}{}, "schedule": map[string]interface{}{},
			"sla":     map[string]interface{}{"p95_ms": 500, "error_rate_max": 0.05},
			"honesty": "JMX import preserved nested controller tree (thread-group metadata unavailable).",
		}, warnings, nil
	}

	var plan jmxTestPlan
	if err := xml.Unmarshal(raw, &plan); err != nil {
		return parseJMXLoose(raw, name)
	}
	var tgs []jmxThreadGroup
	var https []jmxHTTPSampler
	var regs []jmxRegexExtractor
	var csvs []jmxCSVDataSet
	var timers []jmxConstantTimer
	for _, ht := range plan.HashTree {
		walkJMXTree(ht, &tgs, &https, &regs, &csvs, &timers)
	}
	if len(https) == 0 {
		return parseJMXLoose(raw, name)
	}

	vus, dur, ramp := 10, 60, 0
	if len(tgs) > 0 {
		pm := jmxPropMap(tgs[0].Props)
		if n := pm["ThreadGroup.num_threads"]; n != "" {
			fmt.Sscanf(n, "%d", &vus)
		}
		if n := pm["ThreadGroup.ramp_time"]; n != "" {
			fmt.Sscanf(n, "%d", &ramp)
		}
		if n := pm["ThreadGroup.duration"]; n != "" {
			fmt.Sscanf(n, "%d", &dur)
		}
	}
	if dur <= 0 {
		dur = 60
	}

	think := 0
	if len(timers) > 0 {
		tm := jmxPropMap(timers[0].Props)
		fmt.Sscanf(tm["ConstantTimer.delay"], "%d", &think)
	}

	steps := []map[string]interface{}{}
	for i, s := range https {
		pm := jmxPropMap(s.Props)
		domain := pm["HTTPSampler.domain"]
		path := pm["HTTPSampler.path"]
		port := pm["HTTPSampler.port"]
		proto := pm["HTTPSampler.protocol"]
		if proto == "" {
			proto = "http"
		}
		method := pm["HTTPSampler.method"]
		if method == "" {
			method = "GET"
		}
		url := pm["HTTPSampler.path"]
		if domain != "" {
			host := domain
			if port != "" && port != "80" && port != "443" {
				host = domain + ":" + port
			}
			if !strings.HasPrefix(path, "/") && path != "" {
				path = "/" + path
			}
			url = fmt.Sprintf("%s://%s%s", proto, host, path)
		} else if !strings.HasPrefix(url, "http") {
			url = "http://127.0.0.1" + path
		}
		step := map[string]interface{}{
			"type": "http", "name": nz(s.TestName, fmt.Sprintf("step-%d", i+1)),
			"method": method, "url": url, "body": pm["Argument.value"],
		}
		if think > 0 {
			step["think_ms"] = think
		}
		steps = append(steps, step)
	}
	for _, re := range regs {
		pm := jmxPropMap(re.Props)
		ref := pm["RegexExtractor.refname"]
		if ref == "" {
			continue
		}
		steps = append(steps, map[string]interface{}{
			"type": "extract", "name": nz(re.TestName, ref),
			"engine": "regex", "expression": pm["RegexExtractor.regex"],
			"var": ref, "template": pm["RegexExtractor.template"],
		})
	}

	datasets := map[string]interface{}{}
	for _, c := range csvs {
		datasets["csv"] = perfDatasetFromJMXCSVDataSet(c)
	}

	schedule := map[string]interface{}{}
	if ramp > 0 {
		schedule["ramp_seconds"] = ramp
		schedule["profile"] = "ramp"
	}

	firstURL := "http://127.0.0.1:8080/api/health"
	firstMethod := "GET"
	if len(steps) > 0 {
		if u, ok := steps[0]["url"].(string); ok && u != "" {
			firstURL = u
		}
		if m, ok := steps[0]["method"].(string); ok && m != "" {
			firstMethod = m
		}
	}

	out := map[string]interface{}{
		"name":             nz(name, "imported-jmx"),
		"target_url":       firstURL,
		"method":           firstMethod,
		"vus":              vus,
		"duration_seconds": dur,
		"steps":            steps,
		"datasets":         datasets,
		"schedule":         schedule,
		"sla":              map[string]interface{}{"p95_ms": 500, "error_rate_max": 0.05},
		"honesty":          "JMX import is best-effort for HTTP samplers, timers, extractors, CSV, classic thread groups.",
	}
	return out, warnings, nil
}

func firstHTTPFromSteps(steps []map[string]interface{}) (url, method string) {
	url, method = "http://127.0.0.1:8080/api/health", "GET"
	var walk func([]map[string]interface{}) bool
	walk = func(list []map[string]interface{}) bool {
		for _, s := range list {
			if fmt.Sprint(s["type"]) == "http" || s["type"] == nil || fmt.Sprint(s["type"]) == "" {
				if u, ok := s["url"].(string); ok && u != "" {
					url = u
				}
				if m, ok := s["method"].(string); ok && m != "" {
					method = m
				}
				return true
			}
			if walk(stepChildren(s)) {
				return true
			}
		}
		return false
	}
	walk(steps)
	return url, method
}

func parseJMXLoose(raw []byte, name string) (map[string]interface{}, []string, error) {
	warnings := []string{"Used loose JMX parser (structure varied)."}
	s := string(raw)
	steps := []map[string]interface{}{}
	reDomain := regexp.MustCompile(`(?s)<HTTPSamplerProxy[^>]*testname="([^"]*)"[^>]*>.*?<stringProp name="HTTPSampler\.domain">([^<]*)</stringProp>.*?<stringProp name="HTTPSampler\.path">([^<]*)</stringProp>.*?<stringProp name="HTTPSampler\.method">([^<]*)</stringProp>`)
	for _, m := range reDomain.FindAllStringSubmatch(s, -1) {
		path := m[3]
		if !strings.HasPrefix(path, "/") && path != "" {
			path = "/" + path
		}
		url := "http://" + m[2] + path
		if m[2] == "" {
			url = "http://127.0.0.1" + path
		}
		method := m[4]
		if method == "" {
			method = "GET"
		}
		steps = append(steps, map[string]interface{}{
			"type": "http", "name": m[1], "method": method, "url": url,
		})
	}
	if len(steps) == 0 {
		// Minimal placeholder so import still creates a scenario.
		steps = append(steps, map[string]interface{}{
			"type": "http", "name": "health", "method": "GET", "url": "http://127.0.0.1:8080/api/health",
		})
		warnings = append(warnings, "No HTTP samplers matched; inserted health placeholder.")
	}
	first := steps[0]
	return map[string]interface{}{
		"name": nz(name, "imported-jmx"), "target_url": first["url"], "method": first["method"],
		"vus": 10, "duration_seconds": 60, "steps": steps,
		"datasets": map[string]interface{}{}, "schedule": map[string]interface{}{},
		"sla": map[string]interface{}{"p95_ms": 500, "error_rate_max": 0.05},
		"honesty": "JMX import is best-effort for HTTP samplers, timers, extractors, CSV, classic thread groups.",
	}, warnings, nil
}

func handlePerfImportJMX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	name := r.URL.Query().Get("name")
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "multipart/form-data") {
		_ = r.ParseMultipartForm(4 << 20)
		if f, _, ferr := r.FormFile("file"); ferr == nil {
			raw, _ = io.ReadAll(io.LimitReader(f, 4<<20))
			f.Close()
		}
		if name == "" {
			name = r.FormValue("name")
		}
	} else {
		// JSON wrapper { "name", "jmx": "<xml>" } or raw XML
		var wrap struct {
			Name string `json:"name"`
			JMX  string `json:"jmx"`
		}
		if json.Unmarshal(raw, &wrap) == nil && wrap.JMX != "" {
			raw = []byte(wrap.JMX)
			if name == "" {
				name = wrap.Name
			}
		}
	}
	sc, warnings, err := parseJMXToScenario(raw, name)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := loadID("scn", org, proj, fmt.Sprint(sc["name"]), fmt.Sprintf("%d", time.Now().UnixNano()))
	stepsJSON, _ := json.Marshal(sc["steps"])
	dsJSON, _ := json.Marshal(sc["datasets"])
	slaJSON, _ := json.Marshal(sc["sla"])
	schedJSON, _ := json.Marshal(sc["schedule"])
	dataset := perfCSVDatasetFromJSON(string(dsJSON))
	jmxXML := string(raw)
	if !strings.Contains(jmxXML, "jmeterTestPlan") {
		jmxXML = generateJMXFromUpsertData(fmt.Sprint(sc["name"]), fmt.Sprint(sc["target_url"]), fmt.Sprint(sc["method"]), "",
			int(asFloatOr(sc["vus"], 10)), int(asFloatOr(sc["duration_seconds"], 60)), stepsJSON, nil, dataset)
	}
	if jmxContainsUnsafeElements(jmxXML) {
		http.Error(w, "jmx contains unsafe JMeter elements; set OPA_PERF_ALLOW_UNSAFE_JMX=1 to override", 400)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": sc["name"], "target_url": sc["target_url"], "method": sc["method"],
		"vus": sc["vus"], "duration_seconds": sc["duration_seconds"],
		"headers_json": "{}", "body": "", "thresholds_json": string(slaJSON),
		"steps_json": string(stepsJSON), "datasets_json": string(dsJSON),
		"sla_json": string(slaJSON), "schedule_json": string(schedJSON), "jmx_xml": jmxXML,
		"updated_at": now, "created_at": now,
	})
	if writer != nil {
		writer.insertAsync("load_scenarios", append(payload, '\n'))
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "scenario": sc, "warnings": warnings,
		"honesty": "JMeter-compatible designer; Apache JMeter executes stored jmx_xml.",
	})
}

func asFloatOr(v interface{}, def float64) float64 {
	if f, ok := asFloat(v); ok {
		return f
	}
	return def
}

// --- Validate (1 VU dry-run) ---

func handlePerfScenarioValidate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	steps := flattenScenarioSteps(scenarioSteps(sc))
	vars := map[string]string{}
	// Seed the dry run with the first dataset row so parameterised requests are exercised
	// instead of firing literal ${column} text.
	dataset := perfCSVDatasetFromJSON(getString(sc, "datasets_json"))
	seeded := []string{}
	for name, val := range dataset.firstRow() {
		vars[name] = val
		seeded = append(seeded, name)
	}
	sort.Strings(seeded)
	results := []map[string]interface{}{}
	client := perfValidateHTTPClient()
	for _, step := range steps {
		typ, _ := step["type"].(string)
		if typ == "" {
			typ = "http"
		}
		if isPerfControllerMarkerType(typ) {
			// A logic controller is journey structure, not a sampler. Report the shape
			// it was designed with and fire nothing: the steps it wraps are already in
			// the flat list and are exercised on their own.
			results = append(results, perfControllerMarker(typ, step))
			continue
		}
		switch typ {
		case "extract":
			// Apply against last body stored in vars["_body"]
			body := vars["_body"]
			expr, _ := step["expression"].(string)
			vname, _ := step["var"].(string)
			engine, _ := step["engine"].(string)
			val := ""
			if engine == "jsonpath" || strings.HasPrefix(expr, "$.") {
				val = jsonPathSimple(body, expr)
			} else if expr != "" {
				if re, err := safeCompileRegex(expr); err == nil {
					if m := re.FindStringSubmatch(body); len(m) > 1 {
						val = m[1]
					} else if len(m) == 1 {
						val = m[0]
					}
				}
			}
			if vname != "" {
				vars[vname] = val
			}
			results = append(results, map[string]interface{}{
				"type": "extract", "var": vname, "value": truncateStr(val, 200), "ok": val != "",
			})
		case "assert":
			ok := true
			msg := ""
			if st, okSt := step["status"]; okSt {
				want := fmt.Sprintf("%v", st)
				if vars["_status"] != want {
					ok = false
					msg = "status want " + want + " got " + vars["_status"]
				}
			}
			if contains, okC := step["body_contains"].(string); okC && contains != "" {
				if !strings.Contains(vars["_body"], contains) {
					ok = false
					msg = "body_contains missing"
				}
			}
			results = append(results, map[string]interface{}{"type": "assert", "ok": ok, "error": msg})
		case "transaction":
			results = append(results, map[string]interface{}{"type": "transaction", "name": step["name"], "ok": true})
		case "include":
			// Only unresolved references reach here — a resolved one is walked into.
			entry := map[string]interface{}{
				"type": "include", "name": step["name"], "ref": step["ref"], "ok": false,
				"error": nz(fmt.Sprint(step["error"]), "fragment reference did not resolve"),
			}
			results = append(results, entry)
		case "rendezvous":
			rv := perfRendezvousFromStep(step)
			results = append(results, map[string]interface{}{
				"type": "rendezvous", "name": step["name"], "ok": true,
				"group_size": rv.GroupSize, "timeout_ms": rv.TimeoutMS,
				"note": "not exercised by a 1 VU dry run — the synchronising timer only releases once the group fills under real load",
			})
		case "params":
			// Fragment inputs are real bindings: seed them so the reused journey is
			// exercised with its configured values instead of literal ${…} text.
			names, vals := perfStepParams(step)
			for _, n := range names {
				vars[n] = vals[n]
			}
			results = append(results, map[string]interface{}{
				"type": "params", "name": step["name"], "ok": true, "variables": names,
			})
		default: // http
			url := expandPerfVars(fmt.Sprint(step["url"]), vars)
			if url == "" || url == "<nil>" {
				url = expandPerfVars(fmt.Sprint(sc["target_url"]), vars)
			}
			method := strings.ToUpper(nz(fmt.Sprint(step["method"]), "GET"))
			body := expandPerfVars(fmt.Sprint(step["body"]), vars)
			entry := map[string]interface{}{"type": "http", "name": step["name"], "method": method, "url": url}
			if err := isBlockedPerfURL(url); err != nil {
				entry["ok"] = false
				entry["error"] = "url blocked: " + err.Error()
				results = append(results, entry)
				continue
			}
			req, err := http.NewRequest(method, url, strings.NewReader(body))
			if err != nil {
				entry["ok"] = false
				entry["error"] = err.Error()
				results = append(results, entry)
				continue
			}
			if hdrs, ok := step["headers"].(map[string]interface{}); ok {
				for k, v := range hdrs {
					req.Header.Set(k, expandPerfVars(fmt.Sprint(v), vars))
				}
			}
			start := time.Now()
			resp, err := client.Do(req)
			entry["latency_ms"] = float64(time.Since(start).Milliseconds())
			if err != nil {
				entry["ok"] = false
				entry["error"] = err.Error()
				results = append(results, entry)
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			vars["_body"] = string(b)
			vars["_status"] = fmt.Sprintf("%d", resp.StatusCode)
			entry["status_code"] = resp.StatusCode
			entry["ok"] = httpStatusOK2xx(resp.StatusCode)
			entry["body_preview"] = truncateStr(string(b), 300)
			results = append(results, entry)
			if think, ok := step["think_ms"].(float64); ok && think > 0 {
				time.Sleep(time.Duration(think) * time.Millisecond)
			}
		}
	}
	pass, triage := triageValidateResults(results)
	suggestions := suggestAutoCorrelation(results)
	honesty := "1 VU dry-run with triage + auto-correlation suggestions — fix extract/assert/connectivity before dispatching load workers."
	unbound, resolvable := scenarioUnboundVariables(sc)
	if len(unbound) > 0 {
		// A plan that would fire literal ${…} text must not report a clean pass.
		pass = false
		triage = append(triage, unboundVariableTriage(unbound))
		honesty += " Unbound ${…} tokens fail validation instead of burning engine time on literal placeholder requests."
	} else if !resolvable {
		honesty += " Dataset points at a runner-local CSV whose columns are unknown here, so ${…} tokens could not be checked."
	} else {
		honesty += " No unbound ${…} tokens: every reference resolves to a dataset column, an extractor, or a plan built-in."
	}
	// Reusable journeys: say per reference whether the emitted plan points at the shared
	// fragment container or holds an inline copy of it, because the two behave
	// differently under load and the caller cannot tell them apart from the step list.
	tree := scenarioSteps(sc)
	fragRefs := perfFragmentRefs(tree, getString(sc, "name"))
	if h := perfFragmentRefsHonesty(fragRefs); h != "" {
		honesty += " " + h
	}
	for _, ref := range fragRefs {
		if ref.Mode == perfFragmentModeUnresolved {
			pass = false
			triage = append(triage, map[string]interface{}{
				"kind": "fragment_reference", "step": ref.Step, "ref": ref.Ref,
				"error": ref.Reason,
				"fix":   "add a fragment step named " + nz(ref.Ref, "<ref>") + " or point the reference at an existing one",
			})
		}
	}
	// A burst that cannot fill its group parks threads instead of releasing them.
	rvTriage := perfRendezvousTriage(tree, int(asFloatOr(sc["vus"], 0)))
	for _, msg := range rvTriage {
		pass = false
		triage = append(triage, map[string]interface{}{
			"kind": "rendezvous", "error": msg,
			"fix": "lower group_size to at most the scenario VU count, or set timeout_ms",
		})
	}
	if len(rvTriage) > 0 {
		honesty += " A synchronising timer whose group never fills blocks its threads instead of bursting, so validation fails rather than letting the run stall."
	}
	out := map[string]interface{}{
		"ok": pass, "pass": pass, "scenario_id": id, "steps": results,
		"triage": triage, "correlation_suggestions": suggestions,
		"vars": scrubValidateVars(vars),
		"unbound_variables": unbound,
		"honesty": honesty,
	}
	if len(fragRefs) > 0 {
		out["fragment_references"] = fragRefs
	}
	if !resolvable {
		out["dataset_columns_unknown"] = true
	}
	if dataset != nil {
		summary := dataset.summary()
		if len(seeded) > 0 {
			summary["seeded_variables"] = seeded
		}
		out["dataset"] = summary
	}
	writeJSON(w, out)
}

// perfControllerMarker describes a logic controller in validate output. A controller
// decides which of its children run and how often — it is not a request — so the dry
// run reports the branch condition, loop count or input variable it was designed with
// and issues nothing. Apache JMeter evaluates the controller itself under load; the 1 VU
// dry run walks the wrapped steps once each, in flat order, below this entry.
func perfControllerMarker(typ string, step map[string]interface{}) map[string]interface{} {
	entry := map[string]interface{}{"type": typ, "name": step["name"], "ok": true}
	switch typ {
	case "if", "if_controller", "while", "while_controller":
		if cond := strings.TrimSpace(getString(step, "condition")); cond != "" {
			entry["condition"] = cond
		}
	case "loop", "loop_controller":
		if n, ok := asFloat(step["loops"]); ok && int(n) > 0 {
			entry["loops"] = int(n)
		}
		if forever, ok := step["forever"].(bool); ok && forever {
			entry["forever"] = true
		}
	case "foreach", "foreach_controller", "for_each":
		if v := strings.TrimSpace(getString(step, "input_var")); v != "" {
			entry["input_var"] = v
		}
		if v := strings.TrimSpace(getString(step, "return_var")); v != "" {
			entry["return_var"] = v
		}
	}
	entry["note"] = "logic controller — journey structure, not a sample: a 1 VU dry run does not evaluate the branch or iteration count (Apache JMeter does that under load), and the steps it wraps are reported below it"
	return entry
}

func scrubValidateVars(vars map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range vars {
		if k == "_body" {
			out[k] = truncateStr(v, 200)
			continue
		}
		out[k] = v
	}
	return out
}

func expandPerfVars(s string, vars map[string]string) string {
	out := s
	for k, v := range vars {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out = strings.ReplaceAll(out, "${"+k+"}", v)
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

func jsonPathSimple(body, path string) string {
	path = strings.TrimPrefix(path, "$.")
	var m interface{}
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	cur := m
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = obj[part]
	}
	switch t := cur.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func loadScenarioMapReq(r *http.Request, id string) map[string]interface{} {
	if queryClient == nil || id == "" {
		return nil
	}
	owned := perfOwnedAnd(r)
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body,
			thresholds_json, steps_json, datasets_json, sla_json, schedule_json, jmx_xml
		FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s%s LIMIT 1`, escapeSQL(id), owned, scenarioArchivedAnd()))
	if err != nil || len(rows) == 0 {
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body, thresholds_json
			FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), owned))
		if err != nil || len(rows) == 0 {
			return nil
		}
	}
	return rows[0]
}

// loadScenarioMapReqAny loads a scenario including soft-archived rows (for unarchive / restore).
func loadScenarioMapReqAny(r *http.Request, id string) map[string]interface{} {
	if queryClient == nil || id == "" {
		return nil
	}
	owned := perfOwnedAnd(r)
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body,
			thresholds_json, steps_json, datasets_json, sla_json, schedule_json, jmx_xml,
			coalesce(archived, 0) AS archived
		FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), owned))
	if err != nil || len(rows) == 0 {
		return loadScenarioMapReq(r, id)
	}
	return rows[0]
}

// loadScenarioMap is used by background dispatchers with explicit org/proj ownership.
func loadScenarioMap(id string) map[string]interface{} {
	return loadScenarioMapForTenant(id, "", "")
}

func loadScenarioMapForTenant(id, org, proj string) map[string]interface{} {
	if queryClient == nil || id == "" {
		return nil
	}
	owned := ""
	if org != "" && proj != "" {
		owned = fmt.Sprintf(" AND coalesce(nullif(organization_id, ''), '%s') = '%s' AND coalesce(nullif(project_id, ''), '%s') = '%s'",
			defaultOrgID, escapeSQL(org), defaultProjectID, escapeSQL(proj))
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body,
			thresholds_json, steps_json, datasets_json, sla_json, schedule_json, jmx_xml
		FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s%s LIMIT 1`, escapeSQL(id), owned, scenarioArchivedAnd()))
	if err != nil || len(rows) == 0 {
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT id, name, target_url, method, vus, duration_seconds, headers_json, body, thresholds_json
			FROM ` + chTable("load_scenarios") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(id), owned))
		if err != nil || len(rows) == 0 {
			return nil
		}
	}
	return rows[0]
}

func scenarioSteps(sc map[string]interface{}) []map[string]interface{} {
	raw := getString(sc, "steps_json")
	if raw == "" || raw == "[]" {
		// legacy single URL
		return []map[string]interface{}{{
			"type": "http", "name": "main",
			"method": nz(getString(sc, "method"), "GET"),
			"url":    getString(sc, "target_url"),
			"body":   getString(sc, "body"),
		}}
	}
	var steps []map[string]interface{}
	if json.Unmarshal([]byte(raw), &steps) != nil {
		return nil
	}
	return steps
}

// --- Export JMX ---

func handlePerfExportJMX(w http.ResponseWriter, r *http.Request, id string) {
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	dataset := perfCSVDatasetFromJSON(getString(sc, "datasets_json"))
	// Prefer stored jmx_xml (source of truth for Docker runs — includes selector comments, bodies, extractors).
	jmx := strings.TrimSpace(getString(sc, "jmx_xml"))
	if jmx == "" {
		stepsRaw := getString(sc, "steps_json")
		steps := json.RawMessage(stepsRaw)
		if len(steps) == 0 {
			steps = json.RawMessage(`[]`)
		}
		vus := int(getFloat64(sc, "vus"))
		dur := int(getFloat64(sc, "duration_seconds"))
		jmx = generateJMXFromUpsertData(getString(sc, "name"), getString(sc, "target_url"), nz(getString(sc, "method"), "GET"),
			getString(sc, "body"), vus, dur, steps, nil, dataset)
	}
	// Exported plans carry the dataset wiring that matches datasets_json, even when the stored
	// jmx_xml predates CSVDataSet emission or was saved with different columns.
	jmx, _ = syncJMXCSVDataSet(jmx, dataset)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".jmx"))
	_, _ = w.Write([]byte(jmx))
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// xmlCommentSafe strips sequences that would break an XML comment.
func xmlCommentSafe(s string) string {
	s = strings.ReplaceAll(s, "--", "—")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 200 {
		s = s[:197] + "…"
	}
	return s
}

// evaluateSLA delegates to fail-closed evaluation (legacy name kept for callers).
func evaluateSLA(summary map[string]interface{}, sla map[string]interface{}) (pass bool, reasons []string) {
	return evaluateSLAFailClosed(summary, sla)
}

func asFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		var f float64
		_, err := fmt.Sscanf(t, "%f", &f)
		return f, err == nil
	default:
		return 0, false
	}
}

func handlePerfScenarioGate(w http.ResponseWriter, r *http.Request, scenarioID string) {
	// POST with { "run_id": "..." } or GET ?run_id=
	runID := r.URL.Query().Get("run_id")
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.RunID != "" {
			runID = body.RunID
		}
	}
	if runID == "" {
		http.Error(w, "run_id required", 400)
		return
	}
	sc := loadScenarioMapReq(r, scenarioID)
	if sc == nil {
		http.Error(w, "scenario not found", 404)
		return
	}
	sla := map[string]interface{}{}
	if s := getString(sc, "sla_json"); s != "" && s != "{}" {
		_ = json.Unmarshal([]byte(s), &sla)
	} else if s := getString(sc, "thresholds_json"); s != "" {
		_ = json.Unmarshal([]byte(s), &sla)
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, summary_json FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "run not found", 404)
		return
	}
	if getString(rows[0], "scenario_id") != scenarioID {
		http.Error(w, "run does not belong to scenario", 400)
		return
	}
	runStatus := getString(rows[0], "status")
	if strings.EqualFold(runStatus, "created") || strings.EqualFold(runStatus, "cancelled") ||
		strings.EqualFold(runStatus, "canceled") || strings.EqualFold(runStatus, "aborted") {
		writeJSON(w, map[string]interface{}{
			"ok": false, "pass": false, "status": "failed", "run_id": runID, "scenario_id": scenarioID,
			"reasons": []string{"run never executed (status=" + runStatus + ")"}, "run_status": runStatus,
		})
		return
	}
	if !runStatusTerminal(runStatus) {
		writeJSON(w, map[string]interface{}{
			"ok": false, "pass": false, "status": "running", "run_id": runID, "scenario_id": scenarioID,
			"reasons": []string{"run not finished"}, "run_status": runStatus,
		})
		return
	}
	summary := map[string]interface{}{}
	_ = json.Unmarshal([]byte(getString(rows[0], "summary_json")), &summary)
	pass, reasons := evaluateSLAFailClosed(summary, sla)
	status := "passed"
	if !pass {
		status = "failed"
	}
	writeJSON(w, map[string]interface{}{
		"ok": pass, "pass": pass, "status": status, "run_id": runID, "scenario_id": scenarioID,
		"summary": summary, "sla": sla, "reasons": reasons, "run_status": runStatus,
	})
}

func handlePerfRunGate(w http.ResponseWriter, r *http.Request, runID string) {
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, summary_json FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	scenarioID := getString(rows[0], "scenario_id")
	sc := loadScenarioMapReq(r, scenarioID)
	sla := map[string]interface{}{}
	if sc != nil {
		if s := getString(sc, "sla_json"); s != "" && s != "{}" {
			_ = json.Unmarshal([]byte(s), &sla)
		} else if s := getString(sc, "thresholds_json"); s != "" {
			_ = json.Unmarshal([]byte(s), &sla)
		}
	}
	runStatus := getString(rows[0], "status")
	if strings.EqualFold(runStatus, "created") || strings.EqualFold(runStatus, "cancelled") ||
		strings.EqualFold(runStatus, "canceled") || strings.EqualFold(runStatus, "aborted") {
		writeJSON(w, map[string]interface{}{
			"ok": false, "pass": false, "status": "failed", "run_id": runID, "scenario_id": scenarioID,
			"reasons": []string{"run never executed (status=" + runStatus + ")"}, "run_status": runStatus,
		})
		return
	}
	if !runStatusTerminal(runStatus) {
		writeJSON(w, map[string]interface{}{
			"ok": false, "pass": false, "status": "running", "run_id": runID, "scenario_id": scenarioID,
			"reasons": []string{"run not finished"}, "run_status": runStatus,
		})
		return
	}
	summary := map[string]interface{}{}
	_ = json.Unmarshal([]byte(getString(rows[0], "summary_json")), &summary)
	pass, reasons := evaluateSLAFailClosed(summary, sla)
	status := "passed"
	if !pass {
		status = "failed"
	}
	writeJSON(w, map[string]interface{}{
		"ok": pass, "pass": pass, "status": status, "run_id": runID, "scenario_id": scenarioID,
		"summary": summary, "sla": sla, "reasons": reasons, "run_status": runStatus,
	})
}

// maybeDispatchLoadRunner starts load-runner.mjs — DEV ONLY.
// Requires OPA_PERF_ALLOW_NODE_FALLBACK=1 and OPA_PERF_RUNNER=exec|embed.
// Production dispatch path is Docker JMeter (see PerfContainerRunner / DockerRunner).
func maybeDispatchLoadRunner(scenarioID, runID string, vus int, profile, org, proj string) map[string]interface{} {
	if !nodePerfFallbackAllowed() {
		return map[string]interface{}{
			"dispatched": false,
			"error":      "Node load-runner is disabled; production path is Docker JMeter (OPA_PERF_ALLOW_NODE_FALLBACK=1 for local/dev)",
		}
	}
	mode := strings.ToLower(strings.TrimSpace(envOr("OPA_PERF_RUNNER", "")))
	if mode != "exec" && mode != "embed" {
		return map[string]interface{}{
			"dispatched": false,
			"tip":        "Set OPA_PERF_ALLOW_NODE_FALLBACK=1 and OPA_PERF_RUNNER=exec to spawn scripts/load-runner.mjs (dev-only).",
		}
	}
	sc := loadScenarioMapForTenant(scenarioID, org, proj)
	if sc == nil {
		return map[string]interface{}{"dispatched": false, "error": "scenario not found"}
	}
	if err := perfScenarioHTTPURLsBlocked(sc); err != nil {
		return map[string]interface{}{"dispatched": false, "error": "url policy: " + err.Error()}
	}
	steps := scenarioSteps(sc)
	payload := map[string]interface{}{
		"id": scenarioID, "vus": clampPerfVUs(vus),
		"duration_seconds": clampPerfDuration(int(getFloat64(sc, "duration_seconds"))),
		"target_url":       getString(sc, "target_url"),
		"method":           getString(sc, "method"),
		"body":             getString(sc, "body"),
		"steps":            steps,
	}
	if ds := getString(sc, "datasets_json"); ds != "" {
		var d map[string]interface{}
		if json.Unmarshal([]byte(ds), &d) == nil {
			// Strip filename paths — only inline CSV is allowed for Agent-spawned runs.
			if csv, ok := d["csv"].(map[string]interface{}); ok {
				delete(csv, "filename")
				d["csv"] = csv
			}
			payload["datasets"] = d
		}
	}
	if sla := getString(sc, "sla_json"); sla != "" {
		var d interface{}
		_ = json.Unmarshal([]byte(sla), &d)
		payload["sla"] = d
	} else if th := getString(sc, "thresholds_json"); th != "" {
		var d interface{}
		_ = json.Unmarshal([]byte(th), &d)
		payload["sla"] = d
	}
	dir := filepath.Join(os.TempDir(), "opa-perf")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, runID+".json")
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return map[string]interface{}{"dispatched": false, "error": err.Error()}
	}
	script := envOr("OPA_LOAD_RUNNER_PATH", "scripts/load-runner.mjs")
	agentURL := envOr("OPA_PUBLIC_URL", "http://127.0.0.1:"+envOr("PORT", "8080"))
	cmd := exec.Command("node", script, "--scenario", path, "--agent", agentURL, "--run-id", runID, "--profile", profile)
	cmd.Dir = envOr("OPA_AGENT_ROOT", ".")
	cmd.Env = append(os.Environ(),
		"OPA_PERF_RUNNER_TOKEN="+strings.TrimSpace(envOr("OPA_PERF_RUNNER_TOKEN", "")),
	)
	if err := cmd.Start(); err != nil {
		return map[string]interface{}{"dispatched": false, "error": err.Error(), "scenario_file": path}
	}
	go func() { _, _ = cmd.Process.Wait() }()
	return map[string]interface{}{"dispatched": true, "pid": cmd.Process.Pid, "scenario_file": path}
}

func handlePerfRunSamples(w http.ResponseWriter, r *http.Request, runID string) {
	if queryClient == nil {
		writeJSON(w, map[string]interface{}{"samples": []interface{}{}})
		return
	}
	owned, err := queryClient.Query(fmt.Sprintf(`
		SELECT id FROM ` + chTable("load_runs") + ` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(owned) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	since := r.URL.Query().Get("since")
	scope := perfOwnedAnd(r)
	if since != "" {
		scope += fmt.Sprintf(" AND ts >= parseDateTime64BestEffort('%s')", escapeSQL(since))
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT run_id, ts, latency_ms, status_code, ok, url, step_name
		FROM ` + chTable("load_run_samples") + ` WHERE run_id = '%s'%s
		ORDER BY ts ASC LIMIT 2000`, escapeSQL(runID), scope))
	if err != nil {
		rows, err = queryClient.Query(fmt.Sprintf(`
			SELECT run_id, ts, latency_ms, status_code, ok, url
			FROM ` + chTable("load_run_samples") + ` WHERE run_id = '%s'%s
			ORDER BY ts ASC LIMIT 2000`, escapeSQL(runID), scope))
	}
	if err != nil {
		writeJSON(w, map[string]interface{}{"samples": []interface{}{}})
		return
	}
	writeJSON(w, map[string]interface{}{"samples": rows})
}
