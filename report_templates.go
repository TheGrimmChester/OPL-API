package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Report / trend template persistence.
//
// A template is a named, org/project-scoped layout: which widgets to render,
// which metrics to include, and the window to cover. Templates live in the
// product database (opl.report_templates) created by ensurePerfLabSchema; the
// hub database is never written from here.
//
// Routes:
//
//	GET    /api/perf/report-templates              list (tenant scoped)
//	POST   /api/perf/report-templates/upsert       create/update (admin)
//	GET    /api/perf/report-templates/{id}         fetch one
//	DELETE /api/perf/report-templates/{id}         soft archive (admin)
//	POST   /api/perf/report-templates/{id}/archive soft archive (admin)
//
// Exports accept ?template=<id>:
//
//	GET /api/perf/runs/{id}/report?format=…&template=…
//	GET /api/perf/runs/{id}/bench-pack?template=…
//	GET /api/perf/scenarios/{id}/trends?template=…

// reportTemplateWidgets enumerates the widgets a template may select, per kind.
var reportTemplateWidgets = map[string][]string{
	"report": {"kpis", "summary", "steps", "errors", "samples"},
	"trend":  {"kpis", "latency_band", "error_bars", "runs_table"},
}

// reportTemplateMetrics enumerates the selectable metrics (shared by both kinds).
var reportTemplateMetrics = []string{"p50_ms", "p95_ms", "p99_ms", "avg_ms", "error_rate", "samples"}

// reportTemplate is the resolved, validated layout applied to an export.
type reportTemplate struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Kind      string                 `json:"kind"` // report|trend
	Widgets   []string               `json:"widgets"`
	Metrics   []string               `json:"metrics"`
	Window    map[string]interface{} `json:"window"`
	Options   map[string]interface{} `json:"options"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
}

func normalizeTemplateKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "trend", "trends":
		return "trend"
	default:
		return "report"
	}
}

// filterKnown keeps only recognised values, preserving the canonical order so
// exports are stable regardless of the order the operator ticked boxes.
func filterKnown(requested, allowed []string) []string {
	want := map[string]bool{}
	for _, v := range requested {
		want[strings.ToLower(strings.TrimSpace(v))] = true
	}
	out := []string{}
	for _, v := range allowed {
		if want[v] {
			out = append(out, v)
		}
	}
	return out
}

func decodeStringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var single string
	if json.Unmarshal(raw, &single) == nil && single != "" {
		out := []string{}
		for _, p := range strings.Split(single, ",") {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func decodeJSONObject(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		return map[string]interface{}{}
	}
	return out
}

func parseJSONObjectString(s string) map[string]interface{} {
	out := map[string]interface{}{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		return map[string]interface{}{}
	}
	return out
}

func parseJSONStringList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// templateWindowInt reads a numeric window/option value with clamping.
func templateWindowInt(m map[string]interface{}, key string, def, lo, hi int) int {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case int:
		n = t
	case json.Number:
		f, _ := t.Float64()
		n = int(f)
	case string:
		p, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return def
		}
		n = p
	default:
		return def
	}
	if n <= 0 {
		return def
	}
	return clampInt(n, lo, hi)
}

func templateWindowFloat(m map[string]interface{}, key string, def float64) float64 {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok && v != nil {
		f := numFrom(map[string]interface{}{key: v}, key)
		if f > 0 {
			return f
		}
	}
	return def
}

func registerReportTemplateMux(authView, authAdmin func(string, http.HandlerFunc)) {
	authView("/api/perf/report-templates", handleReportTemplates)
	authView("/api/perf/report-templates/", handleReportTemplateSubroutes)
	authAdmin("/api/perf/report-templates/upsert", handleReportTemplateUpsert)
}

func handleReportTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		http.Error(w, "use POST /api/perf/report-templates/upsert (admin)", 405)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		writeJSON(w, map[string]interface{}{"ok": true, "templates": []interface{}{}})
		return
	}
	where := " WHERE coalesce(archived, 0) = 0" + tenantScopeSQL(r, queryClient, "")
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		where += fmt.Sprintf(" AND kind = '%s'", escapeSQL(normalizeTemplateKind(kind)))
	}
	rows, err := queryClient.Query(`
		SELECT id, name, kind, widgets_json, metrics_json, window_json, options_json, created_at, updated_at
		FROM ` + chTable("report_templates") + ` FINAL` + where + `
		ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"ok": true, "templates": []interface{}{},
			"honesty": "No saved templates available yet (table not initialised on this deployment).",
		})
		return
	}
	templates := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		templates = append(templates, reportTemplateRowToJSON(row))
	}
	writeJSON(w, map[string]interface{}{
		"ok":              true,
		"templates":       templates,
		"count":           len(templates),
		"widget_catalog":  reportTemplateWidgets,
		"metric_catalog":  reportTemplateMetrics,
		"honesty":         "Templates select which widgets/metrics/window an export renders. They never change how a run was measured.",
		"scope_honesty":   "Templates are scoped to the requesting organization / project like every other lab object.",
		"applies_to_docs": []string{"runs/{id}/report", "runs/{id}/bench-pack", "scenarios/{id}/trends"},
	})
}

func reportTemplateRowToJSON(row map[string]interface{}) map[string]interface{} {
	kind := normalizeTemplateKind(getString(row, "kind"))
	widgets := filterKnown(parseJSONStringList(getString(row, "widgets_json")), reportTemplateWidgets[kind])
	metrics := filterKnown(parseJSONStringList(getString(row, "metrics_json")), reportTemplateMetrics)
	return map[string]interface{}{
		"id":         getString(row, "id"),
		"name":       getString(row, "name"),
		"kind":       kind,
		"widgets":    widgets,
		"metrics":    metrics,
		"window":     parseJSONObjectString(getString(row, "window_json")),
		"options":    parseJSONObjectString(getString(row, "options_json")),
		"created_at": getString(row, "created_at"),
		"updated_at": getString(row, "updated_at"),
	}
}

func handleReportTemplateSubroutes(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/perf/report-templates/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "id required", 400)
		return
	}
	if parts[0] == "upsert" {
		handleReportTemplateUpsert(w, r)
		return
	}
	id := parts[0]
	if len(parts) > 1 {
		switch parts[1] {
		case "archive":
			handleReportTemplateArchive(w, r, id)
		default:
			http.Error(w, "not found", 404)
		}
		return
	}
	if r.Method == http.MethodDelete {
		handleReportTemplateArchive(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	tpl, err := loadReportTemplate(r, id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"template": tpl,
		"honesty":  "Saved layout only — widget/metric/window selection for exports.",
	})
}

func handleReportTemplateUpsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
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
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	var body struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Kind    string          `json:"kind"`
		Widgets json.RawMessage `json:"widgets"`
		Metrics json.RawMessage `json:"metrics"`
		Window  json.RawMessage `json:"window"`
		Options json.RawMessage `json:"options"`
	}
	if json.Unmarshal(raw, &body) != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name required", 400)
		return
	}
	kind := normalizeTemplateKind(body.Kind)
	widgets := filterKnown(decodeStringList(body.Widgets), reportTemplateWidgets[kind])
	if len(widgets) == 0 {
		widgets = append([]string{}, reportTemplateWidgets[kind]...)
	}
	metrics := filterKnown(decodeStringList(body.Metrics), reportTemplateMetrics)
	if len(metrics) == 0 {
		metrics = append([]string{}, reportTemplateMetrics...)
	}
	window := decodeJSONObject(body.Window)
	options := decodeJSONObject(body.Options)
	name := strings.TrimSpace(body.Name)
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = loadID("rtpl", org, proj, kind, name, fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	widgetsJSON, _ := json.Marshal(widgets)
	metricsJSON, _ := json.Marshal(metrics)
	windowJSON, _ := json.Marshal(window)
	optionsJSON, _ := json.Marshal(options)
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": truncateStr(name, 200), "kind": kind,
		"widgets_json": string(widgetsJSON), "metrics_json": string(metricsJSON),
		"window_json": string(windowJSON), "options_json": string(optionsJSON),
		"archived": 0, "created_at": now, "updated_at": now,
	})
	if !writer.insert("report_templates", append(payload, '\n')) {
		http.Error(w, "template write failed", 502)
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "kind": kind, "name": name,
		"widgets": widgets, "metrics": metrics, "window": window, "options": options,
		"honesty": "Unknown widget/metric names are dropped on save so an export never claims a widget the product cannot render.",
	})
}

func handleReportTemplateArchive(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if writer == nil || queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, organization_id, project_id, name, kind, widgets_json, metrics_json, window_json, options_json, created_at
		FROM `+chTable("report_templates")+` FINAL WHERE id = '%s'%s LIMIT 1`,
		escapeSQL(id), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		http.Error(w, "not found", 404)
		return
	}
	row := rows[0]
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id,
		"organization_id": getString(row, "organization_id"),
		"project_id":      getString(row, "project_id"),
		"name":            getString(row, "name"),
		"kind":            getString(row, "kind"),
		"widgets_json":    getString(row, "widgets_json"),
		"metrics_json":    getString(row, "metrics_json"),
		"window_json":     getString(row, "window_json"),
		"options_json":    getString(row, "options_json"),
		"created_at":      getString(row, "created_at"),
		"archived":        1, "updated_at": now,
	})
	if !writer.insert("report_templates", append(payload, '\n')) {
		http.Error(w, "template write failed", 502)
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "archived": true,
		"honesty": "Soft archive (ReplacingMergeTree) — existing exports that named this template fall back to the full layout.",
	})
}

// loadReportTemplate fetches and validates one template within tenant scope.
func loadReportTemplate(r *http.Request, id string) (*reportTemplate, error) {
	if queryClient == nil {
		return nil, fmt.Errorf("not ready")
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, kind, widgets_json, metrics_json, window_json, options_json, updated_at
		FROM `+chTable("report_templates")+` FINAL
		WHERE id = '%s' AND coalesce(archived, 0) = 0%s LIMIT 1`, escapeSQL(id), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("not found")
	}
	m := reportTemplateRowToJSON(rows[0])
	tpl := &reportTemplate{
		ID:        m["id"].(string),
		Name:      m["name"].(string),
		Kind:      m["kind"].(string),
		Widgets:   m["widgets"].([]string),
		Metrics:   m["metrics"].([]string),
		Window:    m["window"].(map[string]interface{}),
		Options:   m["options"].(map[string]interface{}),
		UpdatedAt: m["updated_at"].(string),
	}
	if len(tpl.Widgets) == 0 {
		tpl.Widgets = append([]string{}, reportTemplateWidgets[tpl.Kind]...)
	}
	if len(tpl.Metrics) == 0 {
		tpl.Metrics = append([]string{}, reportTemplateMetrics...)
	}
	return tpl, nil
}

// resolveExportTemplate reads ?template=<id> and returns the template plus a
// plain note describing what happened (missing ids are reported, not ignored).
func resolveExportTemplate(r *http.Request, wantKind string) (*reportTemplate, string) {
	id := strings.TrimSpace(r.URL.Query().Get("template"))
	if id == "" {
		return nil, ""
	}
	tpl, err := loadReportTemplate(r, id)
	if err != nil {
		return nil, fmt.Sprintf("template %q not found in this scope — rendered the full layout instead", id)
	}
	if wantKind != "" && tpl.Kind != wantKind {
		return nil, fmt.Sprintf("template %q is a %s template — rendered the full %s layout instead", tpl.Name, tpl.Kind, wantKind)
	}
	return tpl, ""
}

func (t *reportTemplate) hasWidget(name string) bool {
	if t == nil {
		return true
	}
	for _, w := range t.Widgets {
		if w == name {
			return true
		}
	}
	return false
}

func (t *reportTemplate) hasMetric(name string) bool {
	if t == nil {
		return true
	}
	for _, m := range t.Metrics {
		if m == name {
			return true
		}
	}
	return false
}

// metricColumns returns the metric keys to render, in canonical order.
func (t *reportTemplate) metricColumns() []string {
	if t == nil {
		return append([]string{}, reportTemplateMetrics...)
	}
	out := append([]string{}, t.Metrics...)
	sort.SliceStable(out, func(i, j int) bool {
		return metricOrderIndex(out[i]) < metricOrderIndex(out[j])
	})
	return out
}

func metricOrderIndex(name string) int {
	for i, m := range reportTemplateMetrics {
		if m == name {
			return i
		}
	}
	return len(reportTemplateMetrics)
}

// describe is embedded into export payloads so a reader can see the layout used.
func (t *reportTemplate) describe() map[string]interface{} {
	if t == nil {
		return nil
	}
	return map[string]interface{}{
		"id": t.ID, "name": t.Name, "kind": t.Kind,
		"widgets": t.Widgets, "metrics": t.Metrics, "window": t.Window,
	}
}

// trendWidgets is the trend widget list a client should render.
// A nil template means "render every trend widget".
func (t *reportTemplate) trendWidgets() []string {
	if t == nil {
		return append([]string{}, reportTemplateWidgets["trend"]...)
	}
	return t.Widgets
}

func (t *reportTemplate) label() string {
	if t == nil {
		return "full layout (no template)"
	}
	return t.Name
}
