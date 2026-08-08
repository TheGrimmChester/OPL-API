package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// JMeter Perf Lab — — HAR / XHR capture import → visual scenario steps (+ optional JMX).
// Selectors on steps correlate recorded UI actions with HTTP samplers; they are
// stored in steps_json and mirrored as JMX XML comments (not executable).

func registerHARCaptureMux(mux *http.ServeMux, authView, authEditor func(string, http.HandlerFunc)) {
	authEditor("/api/perf/scenarios/import-har", handlePerfImportHAR)
	authEditor("/api/perf/scenarios/import-xhr", handlePerfImportXHR)
	_ = mux
	_ = authView
}

type harRoot struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Entries []harEntry `json:"entries"`
}

type harEntry struct {
	StartedDateTime string                 `json:"startedDateTime"`
	Time            float64                `json:"time"`
	Request         harRequest             `json:"request"`
	Response        harResponse            `json:"response"`
	Pageref         string                 `json:"pageref"`
	Opa             map[string]interface{} `json:"_opa"`
}

type harRequest struct {
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Headers  []harNameValue  `json:"headers"`
	Query    []harNameValue  `json:"queryString"`
	PostData *harPostData    `json:"postData"`
	BodySize int             `json:"bodySize"`
}

type harResponse struct {
	Status  int            `json:"status"`
	Headers []harNameValue `json:"headers"`
}

type harNameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Params   []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"params"`
}

// xhrStepInput is a browser XHR / fetch log row (Chrome copy, custom export, etc.).
type xhrStepInput struct {
	Name         string            `json:"name"`
	Method       string            `json:"method"`
	URL          string            `json:"url"`
	Body         string            `json:"body"`
	PostData     string            `json:"postData"`
	Headers      map[string]string `json:"headers"`
	ThinkMs      float64           `json:"think_ms"`
	SelectorType string            `json:"selector_type"`
	Selector     string            `json:"selector"`
	PageURL      string            `json:"page_url"`
	UIAction     string            `json:"ui_action"`
}

func handlePerfImportHAR(w http.ResponseWriter, r *http.Request) {
	handlePerfImportCapture(w, r, "har")
}

func handlePerfImportXHR(w http.ResponseWriter, r *http.Request) {
	handlePerfImportCapture(w, r, "xhr")
}

func handlePerfImportCapture(w http.ResponseWriter, r *http.Request, kind string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj, userID := ctx.WriteOwner()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}

	name := r.URL.Query().Get("name")
	dryRun := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	includeStatic := r.URL.Query().Get("include_static") == "1"
	existingID := r.URL.Query().Get("id")

	var wrap struct {
		Name          string          `json:"name"`
		ID            string          `json:"id"`
		DryRun        bool            `json:"dry_run"`
		IncludeStatic bool            `json:"include_static"`
		HAR           json.RawMessage `json:"har"`
		XHR           json.RawMessage `json:"xhr"`
		Entries       json.RawMessage `json:"entries"`
		Log           json.RawMessage `json:"log"`
	}
	payload := raw
	if json.Unmarshal(raw, &wrap) == nil {
		if wrap.Name != "" && name == "" {
			name = wrap.Name
		}
		if wrap.ID != "" && existingID == "" {
			existingID = wrap.ID
		}
		if wrap.DryRun {
			dryRun = true
		}
		if wrap.IncludeStatic {
			includeStatic = true
		}
		if kind == "har" {
			if len(wrap.HAR) > 0 {
				payload = wrap.HAR
			} else if len(wrap.Log) > 0 {
				// { "log": { "entries": ... } } already at top level — keep raw
				payload = raw
			}
		} else {
			if len(wrap.XHR) > 0 {
				payload = wrap.XHR
			} else if len(wrap.Entries) > 0 {
				payload = wrap.Entries
			}
		}
	}

	var steps []map[string]interface{}
	var warnings []string
	var parseErr error
	if kind == "har" {
		steps, warnings, parseErr = parseHARToSteps(payload, includeStatic)
	} else {
		steps, warnings, parseErr = parseXHRToSteps(payload, includeStatic)
	}
	if parseErr != nil {
		http.Error(w, parseErr.Error(), 400)
		return
	}
	if len(steps) == 0 {
		// Opaque "no HTTP requests" hid why every entry was dropped (e.g. all RFC1918).
		summary := "no skip tallies"
		for _, wmsg := range warnings {
			if strings.HasPrefix(wmsg, "skipped ") && strings.Contains(wmsg, "static=") {
				summary = wmsg
				break
			}
		}
		http.Error(w, "no HTTP requests found in "+kind+" payload ("+summary+")", 400)
		return
	}

	firstURL, firstMethod := "https://example.com/", "GET"
	if u, ok := steps[0]["url"].(string); ok && u != "" {
		firstURL = u
	}
	if m, ok := steps[0]["method"].(string); ok && m != "" {
		firstMethod = m
	}
	scnName := nz(name, "imported-"+kind)
	sc := map[string]interface{}{
		"name":             scnName,
		"target_url":       firstURL,
		"method":           firstMethod,
		"vus":              10,
		"duration_seconds": 60,
		"steps":            steps,
		"datasets":         map[string]interface{}{},
		"schedule":         map[string]interface{}{},
		"sla":              map[string]interface{}{"p95_ms": 500, "error_rate_max": 0.05},
		"honesty":          kind + " import maps browser network traffic to HTTP samplers; UI selectors are correlation metadata.",
	}

	if dryRun {
		out := map[string]interface{}{
			"ok": true, "dry_run": true, "scenario": sc, "steps": steps,
			"count": len(steps), "warnings": warnings,
			"honesty": "dry_run did not persist; POST without dry_run to upsert.",
		}
		for _, wmsg := range warnings {
			if strings.Contains(wmsg, "OPA_PERF_INTERNAL_HOSTS") {
				out["honesty"] = fmt.Sprint(out["honesty"]) + " Lab/private URLs imported with warnings — set OPA_PERF_INTERNAL_HOSTS before validate/dispatch or dial-pin will block them."
				break
			}
		}
		writeJSON(w, out)
		return
	}

	id := existingID
	if id == "" {
		id = loadID("scn", org, proj, scnName, fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	stepsJSON, _ := json.Marshal(steps)
	dsJSON, _ := json.Marshal(sc["datasets"])
	slaJSON, _ := json.Marshal(sc["sla"])
	schedJSON, _ := json.Marshal(sc["schedule"])
	jmxXML := generateJMXFromUpsert(scnName, firstURL, firstMethod, "", 10, 60, stepsJSON)
	if jmxContainsUnsafeElements(jmxXML) {
		http.Error(w, "generated jmx contains unsafe elements", 400)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	row, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj, "user_id": userID,
		"name": scnName, "target_url": firstURL, "method": firstMethod,
		"vus": 10, "duration_seconds": 60,
		"headers_json": "{}", "body": "", "thresholds_json": string(slaJSON),
		"steps_json": string(stepsJSON), "datasets_json": string(dsJSON),
		"sla_json": string(slaJSON), "schedule_json": string(schedJSON), "jmx_xml": jmxXML,
		"updated_at": now, "created_at": now,
	})
	if writer != nil {
		writer.insertAsync("load_scenarios", append(row, '\n'))
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "scenario": sc, "count": len(steps), "warnings": warnings,
		"honesty": harImportHonesty(kind, warnings),
	})
}

func harImportHonesty(kind string, warnings []string) string {
	h := "JMeter-compatible scenario from " + kind + "; jmx_xml is source of truth for Docker JMeter runs."
	for _, wmsg := range warnings {
		if strings.Contains(wmsg, "OPA_PERF_INTERNAL_HOSTS") {
			return h + " Lab/private URLs imported with warnings — set OPA_PERF_INTERNAL_HOSTS before validate/dispatch or dial-pin will block them."
		}
	}
	return h
}

func parseHARToSteps(raw []byte, includeStatic bool) ([]map[string]interface{}, []string, error) {
	var root harRoot
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("invalid HAR JSON: %w", err)
	}
	if len(root.Log.Entries) == 0 {
		// Tolerate bare { "entries": [...] }
		var alt struct {
			Entries []harEntry `json:"entries"`
		}
		if err := json.Unmarshal(raw, &alt); err == nil && len(alt.Entries) > 0 {
			root.Log.Entries = alt.Entries
		}
	}
	if len(root.Log.Entries) == 0 {
		return nil, nil, fmt.Errorf("HAR has no log.entries")
	}

	var warnings []string
	var steps []map[string]interface{}
	var tallies captureImportTallies
	var prevStart time.Time
	labPrivateSeen := false
	for i, e := range root.Log.Entries {
		req := e.Request
		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if method == "" {
			method = "GET"
		}
		u := strings.TrimSpace(req.URL)
		if u == "" {
			tallies.Empty++
			continue
		}
		if !includeStatic && isStaticAssetURL(u) {
			tallies.Static++
			continue
		}
		if method == "OPTIONS" {
			tallies.Options++
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			if err := isObviouslyBlockedPerfURL(u); err != nil {
				tallies.Blocked++
				warnings = append(warnings, fmt.Sprintf("skipped blocked URL %s (%v)", shortURLPath(u), err))
				continue
			}
			if isLabPrivatePerfURL(u) {
				tallies.Private++
				labPrivateSeen = true
			}
		}
		name := fmt.Sprintf("%s %s", method, shortURLPath(u))
		body := ""
		if req.PostData != nil {
			body = req.PostData.Text
			if body == "" && len(req.PostData.Params) > 0 {
				parts := make([]string, 0, len(req.PostData.Params))
				for _, p := range req.PostData.Params {
					parts = append(parts, url.QueryEscape(p.Name)+"="+url.QueryEscape(p.Value))
				}
				body = strings.Join(parts, "&")
			}
		}
		think := 0.0
		if t, err := time.Parse(time.RFC3339Nano, e.StartedDateTime); err == nil {
			if !prevStart.IsZero() {
				d := t.Sub(prevStart).Milliseconds()
				if d > 0 && d < 60000 {
					think = float64(d)
				}
			}
			prevStart = t
		} else if e.StartedDateTime != "" {
			if t, err2 := time.Parse(time.RFC3339, e.StartedDateTime); err2 == nil {
				if !prevStart.IsZero() {
					d := t.Sub(prevStart).Milliseconds()
					if d > 0 && d < 60000 {
						think = float64(d)
					}
				}
				prevStart = t
			}
		}
		step := map[string]interface{}{
			"type": "http", "name": name, "method": method, "url": u, "body": body,
			"think_ms": think, "source": "har", "entry_index": i,
		}
		if e.Pageref != "" {
			step["page_url"] = e.Pageref
		}
		if e.Opa != nil {
			attachSelectorFields(step,
				fmt.Sprint(e.Opa["selector_type"]),
				fmt.Sprint(e.Opa["selector"]),
				fmt.Sprint(e.Opa["page_url"]),
				fmt.Sprint(e.Opa["ui_action"]),
			)
		}
		// Preserve interesting request headers (auth/content-type) as step metadata.
		hdrs := map[string]string{}
		for _, h := range req.Headers {
			ln := strings.ToLower(h.Name)
			if ln == "authorization" || ln == "content-type" || ln == "accept" || ln == "x-csrf-token" || ln == "cookie" {
				hdrs[h.Name] = h.Value
			}
		}
		if len(hdrs) > 0 {
			step["headers"] = hdrs
		}
		steps = append(steps, step)
	}
	warnings = append(warnings, tallies.summaryWarning())
	if labPrivateSeen {
		warnings = append(warnings, "lab/private hosts kept in steps — validate/dispatch still dial-pin via isBlockedPerfURL; set OPA_PERF_INTERNAL_HOSTS to allow them at run time")
	}
	if len(steps) > 200 {
		warnings = append(warnings, fmt.Sprintf("truncated to first 200 of %d requests", len(steps)))
		steps = steps[:200]
	}
	return steps, warnings, nil
}

func parseXHRToSteps(raw []byte, includeStatic bool) ([]map[string]interface{}, []string, error) {
	var list []xhrStepInput
	if err := json.Unmarshal(raw, &list); err != nil {
		// Tolerate { "entries": [...] } / { "xhr": [...] } / single object
		var wrap map[string]json.RawMessage
		if json.Unmarshal(raw, &wrap) == nil {
			for _, key := range []string{"entries", "xhr", "requests", "log"} {
				if b, ok := wrap[key]; ok {
					if json.Unmarshal(b, &list) == nil && len(list) > 0 {
						break
					}
					// HAR-shaped log
					if key == "log" {
						steps, w, e := parseHARToSteps(raw, includeStatic)
						return steps, w, e
					}
				}
			}
		}
		if len(list) == 0 {
			var one xhrStepInput
			if err2 := json.Unmarshal(raw, &one); err2 == nil && one.URL != "" {
				list = []xhrStepInput{one}
			}
		}
		if len(list) == 0 {
			return nil, nil, fmt.Errorf("invalid XHR JSON: expect array of {method,url,...}: %w", err)
		}
	}

	var warnings []string
	var steps []map[string]interface{}
	var tallies captureImportTallies
	labPrivateSeen := false
	for i, x := range list {
		method := strings.ToUpper(strings.TrimSpace(nz(x.Method, "GET")))
		u := strings.TrimSpace(x.URL)
		if u == "" {
			tallies.Empty++
			continue
		}
		if !includeStatic && isStaticAssetURL(u) {
			tallies.Static++
			continue
		}
		if method == "OPTIONS" {
			tallies.Options++
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			if err := isObviouslyBlockedPerfURL(u); err != nil {
				tallies.Blocked++
				warnings = append(warnings, fmt.Sprintf("skipped blocked URL %s", shortURLPath(u)))
				continue
			}
			if isLabPrivatePerfURL(u) {
				tallies.Private++
				labPrivateSeen = true
			}
		}
		body := x.Body
		if body == "" {
			body = x.PostData
		}
		name := x.Name
		if name == "" {
			name = fmt.Sprintf("%s %s", method, shortURLPath(u))
		}
		step := map[string]interface{}{
			"type": "http", "name": name, "method": method, "url": u, "body": body,
			"think_ms": x.ThinkMs, "source": "xhr", "entry_index": i,
		}
		if len(x.Headers) > 0 {
			step["headers"] = x.Headers
		}
		attachSelectorFields(step, x.SelectorType, x.Selector, x.PageURL, x.UIAction)
		steps = append(steps, step)
	}
	warnings = append(warnings, tallies.summaryWarning())
	if labPrivateSeen {
		warnings = append(warnings, "lab/private hosts kept in steps — validate/dispatch still dial-pin via isBlockedPerfURL; set OPA_PERF_INTERNAL_HOSTS to allow them at run time")
	}
	if len(steps) > 200 {
		warnings = append(warnings, fmt.Sprintf("truncated to first 200 of %d requests", len(steps)))
		steps = steps[:200]
	}
	return steps, warnings, nil
}

func attachSelectorFields(step map[string]interface{}, typ, sel, page, action string) {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "<nil>" {
			return ""
		}
		return s
	}
	typ = strings.ToLower(clean(typ))
	sel = clean(sel)
	page = clean(page)
	action = clean(action)
	if typ != "" && typ != "css" && typ != "xpath" && typ != "correlate" {
		typ = "css"
	}
	if typ != "" {
		step["selector_type"] = typ
	}
	if sel != "" {
		step["selector"] = sel
		if typ == "" {
			step["selector_type"] = "css"
		}
	}
	if page != "" {
		step["page_url"] = page
	}
	if action != "" {
		step["ui_action"] = action
	}
}

func isStaticAssetURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case ".css", ".js", ".mjs", ".map", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico",
		".woff", ".woff2", ".ttf", ".eot", ".mp4", ".webm", ".mp3", ".avi":
		return true
	}
	// data: and chrome-extension:
	if u.Scheme == "data" || u.Scheme == "chrome-extension" || u.Scheme == "blob" {
		return true
	}
	return false
}

// captureImportTallies counts why HAR/XHR entries were dropped (or kept as lab-private).
// Private is counted when a lab RFC1918/loopback/host.docker.internal URL is *kept*
// with a warning — it is not a skip. Empty-import errors still surface the tally so
// operators can see how much of the capture was lab traffic vs blocked/static.
type captureImportTallies struct {
	Static  int
	Options int
	Blocked int
	Empty   int
	Private int // lab-private kept (not skipped)
}

func (t captureImportTallies) summaryWarning() string {
	return fmt.Sprintf("skipped static=%d private=%d OPTIONS=%d blocked=%d empty=%d",
		t.Static, t.Private, t.Options, t.Blocked, t.Empty)
}

// isObviouslyBlockedPerfURL hard-blocks only cloud metadata / weird hosts during capture
// import (no DNS). Lab RFC1918, loopback, and host.docker.internal are allowed into steps
// with a warning — full dial-pinned policy still applies at validate/dispatch via
// isBlockedPerfURL (set OPA_PERF_INTERNAL_HOSTS for those hosts at run time).
func isObviouslyBlockedPerfURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme not allowed")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return fmt.Errorf("missing host")
	}
	if host == "metadata.google.internal" ||
		(strings.HasSuffix(host, ".internal") && host != "host.docker.internal") {
		return fmt.Errorf("metadata/internal host not allowed")
	}
	if isWeirdPerfHostForm(host) {
		return fmt.Errorf("encoded/numeric host not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isImportHardBlockedIP(ip) {
			return fmt.Errorf("metadata/link-local address not allowed")
		}
	}
	return nil
}

// isLabPrivatePerfURL reports RFC1918 / loopback / localhost / host.docker.internal —
// import keeps these with a warning; validate still requires OPA_PERF_INTERNAL_HOSTS.
func isLabPrivatePerfURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return false
	}
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return false
}

// isImportHardBlockedIP blocks cloud metadata / link-local / multicast at import time.
// Unlike ipBlockedForPerf, it does not reject RFC1918 or loopback.
func isImportHardBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

func shortURLPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		if len(rawURL) > 48 {
			return rawURL[:48] + "…"
		}
		return rawURL
	}
	p := u.Path
	if len(p) > 64 {
		p = p[:61] + "…"
	}
	return p
}

func handlePerfExportXHR(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	entries := []map[string]interface{}{}
	for _, step := range scenarioSteps(sc) {
		typ, _ := step["type"].(string)
		if typ == "" {
			typ = "http"
		}
		if typ != "http" {
			continue
		}
		entries = append(entries, map[string]interface{}{
			"name":          stepString(step, "name"),
			"method":        nz(stepString(step, "method"), "GET"),
			"url":           stepString(step, "url"),
			"body":          stepString(step, "body"),
			"headers":       step["headers"],
			"think_ms":      step["think_ms"],
			"selector_type": stepString(step, "selector_type"),
			"selector":      stepString(step, "selector"),
			"page_url":      stepString(step, "page_url"),
			"ui_action":     stepString(step, "ui_action"),
		})
	}
	name := sanitizePerfExportName(getString(sc, "name"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-xhr.json"`, name))
	writeJSON(w, map[string]interface{}{
		"format":  "opa-perf-xhr-v1",
		"name":    getString(sc, "name"),
		"entries": entries,
	})
}

func handlePerfExportHAR(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	entries := []map[string]interface{}{}
	for _, step := range scenarioSteps(sc) {
		typ, _ := step["type"].(string)
		if typ == "" {
			typ = "http"
		}
		if typ != "http" {
			continue
		}
		method := nz(stepString(step, "method"), "GET")
		u := stepString(step, "url")
		body := stepString(step, "body")
		var postData interface{}
		if body != "" {
			postData = map[string]interface{}{"mimeType": "application/json", "text": body}
		}
		entry := map[string]interface{}{
			"request": map[string]interface{}{
				"method": method, "url": u, "headers": []interface{}{},
				"queryString": []interface{}{}, "postData": postData,
			},
			"response": map[string]interface{}{
				"status": 0, "statusText": "", "headers": []interface{}{},
				"content": map[string]interface{}{"size": 0, "mimeType": ""},
			},
			"_opa": map[string]interface{}{
				"selector_type": stepString(step, "selector_type"),
				"selector":      stepString(step, "selector"),
				"ui_action":     stepString(step, "ui_action"),
			},
		}
		if page := stepString(step, "page_url"); page != "" {
			entry["pageref"] = page
		}
		entries = append(entries, entry)
	}
	name := sanitizePerfExportName(getString(sc, "name"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.har"`, name))
	writeJSON(w, map[string]interface{}{
		"log": map[string]interface{}{
			"version": "1.2",
			"creator": map[string]string{"name": "OPA Perf Lab", "version": "1"},
			"entries": entries,
		},
	})
}

func sanitizePerfExportName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "scenario"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "scenario"
	}
	return out
}

// stepString reads a step map string field, treating missing/nil/"<nil>" as empty.
func stepString(step map[string]interface{}, key string) string {
	v, ok := step[key]
	if !ok || v == nil {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "<nil>" {
		return ""
	}
	return s
}
