package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Postman Collection v2 / v2.1 → OPL HTTP steps (best-effort).

func registerPostmanMux(_ *http.ServeMux, _, authAdmin func(string, http.HandlerFunc)) {
	authAdmin("/api/perf/scenarios/import-postman", handlePerfImportPostman)
}

func handlePerfImportPostman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}

	name := r.URL.Query().Get("name")
	dryRun := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	existingID := r.URL.Query().Get("id")

	var wrap struct {
		Name     string          `json:"name"`
		ID       string          `json:"id"`
		DryRun   bool            `json:"dry_run"`
		Postman  json.RawMessage `json:"postman"`
		Collection json.RawMessage `json:"collection"`
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
		if len(wrap.Postman) > 0 {
			payload = wrap.Postman
		} else if len(wrap.Collection) > 0 {
			payload = wrap.Collection
		}
	}

	steps, collName, warnings, parseErr := parsePostmanToSteps(payload)
	if parseErr != nil {
		http.Error(w, parseErr.Error(), 400)
		return
	}
	if len(steps) == 0 {
		http.Error(w, "no HTTP requests found in Postman collection", 400)
		return
	}

	firstURL, firstMethod := "https://example.com/", "GET"
	if u, ok := steps[0]["url"].(string); ok && u != "" {
		firstURL = u
	}
	if m, ok := steps[0]["method"].(string); ok && m != "" {
		firstMethod = m
	}
	scnName := nz(name, nz(collName, "imported-postman"))
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
		"honesty":          "Postman collection → HTTP steps; scripts/auth helpers are not executed — map tokens via extractors.",
	}

	if dryRun {
		writeJSON(w, map[string]interface{}{
			"ok": true, "dry_run": true, "scenario": sc, "steps": steps,
			"count": len(steps), "warnings": warnings,
			"honesty": "dry_run did not persist; POST without dry_run to upsert.",
		})
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
		"id": id, "organization_id": org, "project_id": proj,
		"name": scnName, "target_url": firstURL, "method": firstMethod,
		"vus": 10, "duration_seconds": 60,
		"headers_json": "{}", "body": "", "thresholds_json": string(slaJSON),
		"steps_json": string(stepsJSON), "datasets_json": string(dsJSON),
		"sla_json": string(slaJSON), "schedule_json": string(schedJSON), "jmx_xml": jmxXML,
		"archived": 0, "updated_at": now, "created_at": now,
	})
	if writer != nil {
		writer.insertAsync("load_scenarios", append(row, '\n'))
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "scenario": sc, "count": len(steps), "warnings": warnings,
		"honesty": "JMeter-compatible scenario from Postman; jmx_xml is source of truth for Docker JMeter runs.",
	})
}

type postmanCollection struct {
	Info struct {
		Name string `json:"name"`
	} `json:"info"`
	Item []postmanItem `json:"item"`
}

type postmanItem struct {
	Name    string                 `json:"name"`
	Request json.RawMessage        `json:"request"`
	Item    []postmanItem          `json:"item"`
	Event   []json.RawMessage      `json:"event"`
}

type postmanRequest struct {
	Method string          `json:"method"`
	Header []postmanHeader `json:"header"`
	Body   *postmanBody    `json:"body"`
	URL    json.RawMessage `json:"url"`
}

type postmanHeader struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Disabled bool `json:"disabled"`
}

type postmanBody struct {
	Mode       string `json:"mode"`
	Raw        string `json:"raw"`
	URLEncoded []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"urlencoded"`
}

func parsePostmanToSteps(raw []byte) (steps []map[string]interface{}, collName string, warnings []string, err error) {
	var coll postmanCollection
	if e := json.Unmarshal(raw, &coll); e != nil {
		return nil, "", nil, fmt.Errorf("invalid Postman JSON: %w", e)
	}
	if len(coll.Item) == 0 {
		// Sometimes wrapped as { collection: {...} } already unwrapped by caller
		return nil, "", nil, fmt.Errorf("collection has no items")
	}
	collName = coll.Info.Name
	var walk func(items []postmanItem, folder string)
	walk = func(items []postmanItem, folder string) {
		for _, it := range items {
			if len(it.Item) > 0 {
				walk(it.Item, joinPostmanPath(folder, it.Name))
				continue
			}
			if len(it.Request) == 0 {
				continue
			}
			var req postmanRequest
			if json.Unmarshal(it.Request, &req) != nil {
				// request may be a plain URL string
				var urlStr string
				if json.Unmarshal(it.Request, &urlStr) == nil && urlStr != "" {
					step := map[string]interface{}{
						"type": "http", "name": nz(it.Name, "Request"),
						"method": "GET", "url": expandPostmanVars(urlStr), "headers": map[string]interface{}{},
					}
					if e := isBlockedPerfURLLoose(expandPostmanVars(urlStr)); e != nil {
						warnings = append(warnings, fmt.Sprintf("skipped %s: %v", it.Name, e))
						continue
					}
					steps = append(steps, step)
				}
				continue
			}
			urlStr := postmanURLString(req.URL)
			urlStr = expandPostmanVars(urlStr)
			if urlStr == "" {
				warnings = append(warnings, "skip empty URL: "+it.Name)
				continue
			}
			if e := isBlockedPerfURLLoose(urlStr); e != nil {
				warnings = append(warnings, fmt.Sprintf("skipped %s: %v", it.Name, e))
				continue
			}
			method := strings.ToUpper(nz(req.Method, "GET"))
			headers := map[string]interface{}{}
			for _, h := range req.Header {
				if h.Disabled || strings.TrimSpace(h.Key) == "" {
					continue
				}
				headers[h.Key] = expandPostmanVars(h.Value)
			}
			body := ""
			if req.Body != nil {
				switch req.Body.Mode {
				case "raw", "":
					body = expandPostmanVars(req.Body.Raw)
				case "urlencoded":
					parts := make([]string, 0, len(req.Body.URLEncoded))
					for _, p := range req.Body.URLEncoded {
						parts = append(parts, p.Key+"="+expandPostmanVars(p.Value))
					}
					body = strings.Join(parts, "&")
					if _, ok := headers["Content-Type"]; !ok {
						headers["Content-Type"] = "application/x-www-form-urlencoded"
					}
				default:
					warnings = append(warnings, "body mode "+req.Body.Mode+" not mapped for "+it.Name)
				}
			}
			name := it.Name
			if folder != "" {
				name = folder + " / " + name
			}
			steps = append(steps, map[string]interface{}{
				"type": "http", "name": nz(name, "Request"),
				"method": method, "url": urlStr, "body": body, "headers": headers, "children": []interface{}{},
			})
			if len(it.Event) > 0 {
				warnings = append(warnings, "scripts ignored for "+it.Name+" — add extractors manually or use Validate auto-correlation")
			}
		}
	}
	walk(coll.Item, "")
	return steps, collName, warnings, nil
}

func joinPostmanPath(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " / " + b
}

func postmanURLString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Raw  string   `json:"raw"`
		Host []string `json:"host"`
		Path []string `json:"path"`
		Protocol string `json:"protocol"`
		Query []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"query"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Raw != "" {
		return obj.Raw
	}
	proto := nz(obj.Protocol, "https")
	host := strings.Join(obj.Host, ".")
	path := "/" + strings.Join(obj.Path, "/")
	path = strings.ReplaceAll(path, "//", "/")
	q := ""
	if len(obj.Query) > 0 {
		parts := make([]string, 0, len(obj.Query))
		for _, qq := range obj.Query {
			parts = append(parts, qq.Key+"="+qq.Value)
		}
		q = "?" + strings.Join(parts, "&")
	}
	if host == "" {
		return path + q
	}
	return proto + "://" + host + path + q
}

// expandPostmanVars maps {{var}} → ${var} for OPL/JMeter var style.
func expandPostmanVars(s string) string {
	out := s
	// Keep unresolved {{x}} as ${x} so validate/JMeter can substitute later.
	for {
		i := strings.Index(out, "{{")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "}}")
		if j < 0 {
			break
		}
		j += i
		name := strings.TrimSpace(out[i+2 : j])
		out = out[:i] + "${" + name + "}" + out[j+2:]
	}
	return out
}

// isBlockedPerfURLLoose matches HAR import: hard-block metadata/weird only; lab private
// hosts are kept (validate/dispatch still dial-pin via isBlockedPerfURL).
func isBlockedPerfURLLoose(url string) error {
	if strings.Contains(url, "${") {
		return nil
	}
	return isObviouslyBlockedPerfURL(url)
}
