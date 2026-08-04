package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// buildPerfRunReport loads run + samples and returns the structured bench report map.
// Raw samples are stashed under "_samples" for the samples widget; applyReportTemplate
// removes that key before the payload is written.
func buildPerfRunReport(r *http.Request, runID string) (map[string]interface{}, error) {
	if queryClient == nil {
		return nil, fmt.Errorf("not ready")
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, status, vus, started_at, finished_at, summary_json, error
		FROM `+chTable("load_runs")+` FINAL WHERE id = '%s'%s LIMIT 1`, escapeSQL(runID), perfOwnedAnd(r)))
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("not found")
	}
	run := rows[0]
	samples := loadRunSampleMaps(r, runID, 5000)
	if samples == nil {
		samples = []map[string]interface{}{}
	}
	steps := aggregateRunSteps(samples)
	summary := map[string]interface{}{}
	_ = json.Unmarshal([]byte(getString(run, "summary_json")), &summary)
	return map[string]interface{}{
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
		"_samples":     samples,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"honesty":      "Structured bench report (JSON/CSV/HTML/PDF). Bench pack ZIP bundles the same artifacts for offline sharing.",
	}, nil
}

func reportSteps(report map[string]interface{}) []map[string]interface{} {
	raw, _ := report["steps"].([]map[string]interface{})
	if raw != nil {
		return raw
	}
	// JSON round-trip may yield []interface{}
	arr, _ := report["steps"].([]interface{})
	out := make([]map[string]interface{}, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

// --- Template application ---

// reportStepMetricKeys maps a template metric to the per-step aggregate key.
var reportStepMetricKeys = map[string]string{
	"p50_ms": "p50_ms", "p95_ms": "p95_ms", "p99_ms": "p99_ms",
	"avg_ms": "avg_ms", "error_rate": "error_rate", "samples": "samples",
}

// applyReportTemplate trims the report to the widgets/metrics a template selects.
// A nil template leaves the full layout untouched. The template that was applied —
// or the plain reason it was not — is always embedded in the payload.
func applyReportTemplate(report map[string]interface{}, tpl *reportTemplate, note string) {
	samples, _ := report["_samples"].([]map[string]interface{})
	delete(report, "_samples")
	if note != "" {
		report["template_note"] = note
	}
	if tpl == nil {
		report["template"] = nil
		return
	}
	report["template"] = tpl.describe()
	metrics := tpl.metricColumns()
	steps := reportSteps(report)

	if tpl.hasWidget("kpis") {
		sum := summaryMap(report)
		kpis := map[string]interface{}{"requests": numFrom(sum, "requests", "samples", "n")}
		for _, m := range metrics {
			kpis[m] = round2(numFrom(sum, m))
		}
		report["kpis"] = kpis
	}
	if !tpl.hasWidget("summary") {
		delete(report, "summary")
	}
	if tpl.hasWidget("steps") {
		report["steps"] = filterStepMetrics(steps, metrics)
	} else {
		delete(report, "steps")
	}
	if tpl.hasWidget("errors") {
		report["errors"] = reportErrorBreakdown(report, steps)
	}
	if tpl.hasWidget("samples") {
		limit := templateWindowInt(tpl.Options, "sample_cap", 200, 1, 5000)
		limit = templateWindowInt(tpl.Window, "sample_cap", limit, 1, 5000)
		if len(samples) > limit {
			samples = samples[:limit]
		}
		report["samples"] = samples
		report["samples_capped_at"] = limit
	}
	report["honesty"] = fmt.Sprintf("%s Layout from template %q (widgets: %s; metrics: %s) — the selection changes what is rendered, never how the run was measured.",
		report["honesty"], tpl.Name, strings.Join(tpl.Widgets, ","), strings.Join(metrics, ","))
}

// filterStepMetrics keeps identity columns plus the selected metric columns.
func filterStepMetrics(steps []map[string]interface{}, metrics []string) []map[string]interface{} {
	keep := map[string]bool{"step_name": true, "url": true, "errors": true}
	for _, m := range metrics {
		if k, ok := reportStepMetricKeys[m]; ok {
			keep[k] = true
		}
	}
	out := make([]map[string]interface{}, 0, len(steps))
	for _, st := range steps {
		row := map[string]interface{}{}
		for k, v := range st {
			if keep[k] {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}

// reportErrorBreakdown lists the failing steps, worst first, plus the run error.
func reportErrorBreakdown(report map[string]interface{}, steps []map[string]interface{}) map[string]interface{} {
	failing := []map[string]interface{}{}
	for _, st := range steps {
		if numFrom(st, "errors") > 0 {
			failing = append(failing, map[string]interface{}{
				"step_name":  fmt.Sprint(st["step_name"]),
				"errors":     numFrom(st, "errors"),
				"samples":    numFrom(st, "samples"),
				"error_rate": numFrom(st, "error_rate"),
				"url":        fmt.Sprint(st["url"]),
			})
		}
	}
	sort.SliceStable(failing, func(i, j int) bool {
		return numFrom(failing[i], "errors") > numFrom(failing[j], "errors")
	})
	out := map[string]interface{}{
		"failing_steps": failing,
		"count":         len(failing),
	}
	if msg := fmt.Sprint(report["error"]); msg != "" && msg != "<nil>" {
		out["run_error"] = msg
	}
	if len(failing) == 0 {
		out["honesty"] = "No step recorded an error in the captured samples."
	}
	return out
}

// --- CSV ---

// reportCSVColumns keeps CSV in step with the template selection.
// A nil template keeps the full column set.
func reportCSVColumns(tpl *reportTemplate) []string {
	if tpl == nil {
		return []string{"step_name", "samples", "errors", "error_rate", "avg_ms", "p50_ms", "p95_ms", "p99_ms", "min_ms", "max_ms", "url"}
	}
	cols := []string{"step_name"}
	for _, m := range tpl.metricColumns() {
		if k, ok := reportStepMetricKeys[m]; ok {
			cols = append(cols, k)
		}
	}
	return append(cols, "errors", "url")
}

func writeReportCSVRows(cw *csv.Writer, steps []map[string]interface{}, tpl *reportTemplate) {
	cols := reportCSVColumns(tpl)
	_ = cw.Write(cols)
	for _, st := range steps {
		row := make([]string, 0, len(cols))
		for _, c := range cols {
			v, ok := st[c]
			if !ok || v == nil {
				row = append(row, "")
				continue
			}
			row = append(row, fmt.Sprint(v))
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

func writeReportCSV(w http.ResponseWriter, runID string, steps []map[string]interface{}, tpl *reportTemplate) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"opl-report-%s.csv\"", runID))
	writeReportCSVRows(csv.NewWriter(w), steps, tpl)
}

func reportCSVBytes(steps []map[string]interface{}, tpl *reportTemplate) []byte {
	var buf bytes.Buffer
	writeReportCSVRows(csv.NewWriter(&buf), steps, tpl)
	return buf.Bytes()
}

func summaryMap(report map[string]interface{}) map[string]interface{} {
	if m, ok := report["summary"].(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func numFrom(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case int:
				return float64(t)
			case int64:
				return float64(t)
			case json.Number:
				f, _ := t.Float64()
				return f
			case string:
				f, _ := strconv.ParseFloat(t, 64)
				return f
			}
		}
	}
	return 0
}

// reportKPIValue reads a KPI from the template-built kpis block when present,
// falling back to the raw summary so the full layout keeps working.
func reportKPIValue(report map[string]interface{}, key string) float64 {
	if kpis, ok := report["kpis"].(map[string]interface{}); ok {
		if _, has := kpis[key]; has {
			return numFrom(kpis, key)
		}
	}
	if key == "requests" {
		return numFrom(summaryMap(report), "requests", "samples", "n")
	}
	return numFrom(summaryMap(report), key)
}

// reportMetricLabels maps a metric key to a short column/KPI label.
var reportMetricLabels = map[string]string{
	"p50_ms": "p50 ms", "p95_ms": "p95 ms", "p99_ms": "p99 ms",
	"avg_ms": "avg ms", "error_rate": "Error rate", "samples": "Samples",
}

func reportMetricLabel(key string) string {
	if l, ok := reportMetricLabels[key]; ok {
		return l
	}
	return key
}

// --- HTML ---

func renderReportHTML(report map[string]interface{}, tpl *reportTemplate) []byte {
	steps := reportSteps(report)
	runID := fmt.Sprint(report["run_id"])
	metrics := tpl.metricColumns()
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"/>`)
	b.WriteString(`<title>OPL bench report — ` + esc(runID) + `</title>`)
	b.WriteString(`<style>
body{font-family:system-ui,-apple-system,sans-serif;margin:24px;color:#111;background:#fff}
h1{font-size:22px;margin:0 0 4px} .meta{color:#555;font-size:13px;margin-bottom:20px}
.kpis{display:flex;flex-wrap:wrap;gap:12px;margin-bottom:20px}
.kpi{border:1px solid #ddd;border-radius:8px;padding:10px 14px;min-width:100px}
.kpi .l{font-size:11px;text-transform:uppercase;color:#666}.kpi .v{font-size:20px;font-weight:600;font-variant-numeric:tabular-nums}
table{border-collapse:collapse;width:100%;font-size:12px} th,td{border:1px solid #ddd;padding:6px 8px;text-align:left}
th{background:#f4f4f6} td.n{text-align:right;font-variant-numeric:tabular-nums}
h2{font-size:16px}
dl.summary{font-size:12px;display:grid;grid-template-columns:auto 1fr;gap:2px 12px;margin:0 0 20px}
dl.summary dt{color:#555} dl.summary dd{margin:0;font-variant-numeric:tabular-nums}
.honesty{margin-top:24px;font-size:11px;color:#666;border-top:1px solid #eee;padding-top:12px}
@media print{body{margin:12mm}}
</style></head><body>`)
	b.WriteString(`<h1>Open Perf Lab — bench report</h1>`)
	b.WriteString(`<div class="meta">Run <strong>` + esc(runID) + `</strong> · status ` + esc(fmt.Sprint(report["status"])) +
		` · scenario ` + esc(fmt.Sprint(report["scenario_id"])) +
		` · generated ` + esc(fmt.Sprint(report["generated_at"])) +
		` · layout ` + esc(tpl.label()) + `</div>`)

	if tpl.hasWidget("kpis") {
		b.WriteString(`<div class="kpis">`)
		b.WriteString(`<div class="kpi"><div class="l">Requests</div><div class="v">` +
			esc(fmt.Sprintf("%g", reportKPIValue(report, "requests"))) + `</div></div>`)
		for _, m := range metrics {
			b.WriteString(`<div class="kpi"><div class="l">` + esc(reportMetricLabel(m)) + `</div><div class="v">` +
				esc(fmt.Sprintf("%g", reportKPIValue(report, m))) + `</div></div>`)
		}
		b.WriteString(`</div>`)
	}

	if tpl.hasWidget("summary") {
		if sum := summaryMap(report); len(sum) > 0 {
			keys := make([]string, 0, len(sum))
			for k := range sum {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString(`<h2>Run summary</h2><dl class="summary">`)
			for _, k := range keys {
				b.WriteString(`<dt>` + esc(k) + `</dt><dd>` + esc(fmt.Sprint(sum[k])) + `</dd>`)
			}
			b.WriteString(`</dl>`)
		}
	}

	if errMsg := fmt.Sprint(report["error"]); errMsg != "" && errMsg != "<nil>" {
		b.WriteString(`<p><strong>Error:</strong> ` + esc(errMsg) + `</p>`)
	}

	if tpl.hasWidget("steps") {
		b.WriteString(`<h2>Per-step stats</h2><table><thead><tr><th>Step</th>`)
		for _, m := range metrics {
			b.WriteString(`<th>` + esc(reportMetricLabel(m)) + `</th>`)
		}
		b.WriteString(`<th>Errors</th><th>URL</th></tr></thead><tbody>`)
		for _, st := range steps {
			b.WriteString(`<tr><td>` + esc(fmt.Sprint(st["step_name"])) + `</td>`)
			for _, m := range metrics {
				b.WriteString(`<td class="n">` + esc(fmt.Sprint(st[reportStepMetricKeys[m]])) + `</td>`)
			}
			b.WriteString(`<td class="n">` + esc(fmt.Sprint(st["errors"])) + `</td>`)
			b.WriteString(`<td>` + esc(fmt.Sprint(st["url"])) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}

	if tpl.hasWidget("errors") {
		br := reportErrorBreakdown(report, steps)
		failing, _ := br["failing_steps"].([]map[string]interface{})
		b.WriteString(`<h2>Errors</h2>`)
		if len(failing) == 0 {
			b.WriteString(`<p>No step recorded an error in the captured samples.</p>`)
		} else {
			b.WriteString(`<table><thead><tr><th>Step</th><th>Errors</th><th>Samples</th><th>Err rate</th><th>URL</th></tr></thead><tbody>`)
			for _, f := range failing {
				b.WriteString(`<tr><td>` + esc(fmt.Sprint(f["step_name"])) + `</td>`)
				for _, k := range []string{"errors", "samples", "error_rate"} {
					b.WriteString(`<td class="n">` + esc(fmt.Sprintf("%g", numFrom(f, k))) + `</td>`)
				}
				b.WriteString(`<td>` + esc(fmt.Sprint(f["url"])) + `</td></tr>`)
			}
			b.WriteString(`</tbody></table>`)
		}
	}

	if tpl.hasWidget("samples") {
		if rows, ok := report["samples"].([]map[string]interface{}); ok && len(rows) > 0 {
			b.WriteString(fmt.Sprintf(`<h2>Samples (%d shown)</h2>`, len(rows)))
			b.WriteString(`<table><thead><tr><th>ts</th><th>step</th><th>latency ms</th><th>status</th><th>ok</th><th>URL</th></tr></thead><tbody>`)
			for _, s := range rows {
				b.WriteString(`<tr><td>` + esc(getString(s, "ts")) + `</td><td>` + esc(getString(s, "step_name")) + `</td>`)
				b.WriteString(`<td class="n">` + esc(fmt.Sprintf("%g", numFrom(s, "latency_ms"))) + `</td>`)
				b.WriteString(`<td class="n">` + esc(fmt.Sprintf("%g", numFrom(s, "status_code"))) + `</td>`)
				b.WriteString(`<td class="n">` + esc(fmt.Sprintf("%g", numFrom(s, "ok"))) + `</td>`)
				b.WriteString(`<td>` + esc(getString(s, "url")) + `</td></tr>`)
			}
			b.WriteString(`</tbody></table>`)
		}
	}

	b.WriteString(`<p class="honesty">` + esc(fmt.Sprint(report["honesty"])))
	if note := fmt.Sprint(report["template_note"]); note != "" && note != "<nil>" {
		b.WriteString(` ` + esc(note))
	}
	b.WriteString(`</p></body></html>`)
	return []byte(b.String())
}

// --- Minimal PDF 1.4 writer (Helvetica, multi-page text tables) ---

func pdfEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	return s
}

func renderReportPDF(report map[string]interface{}, tpl *reportTemplate) []byte {
	steps := reportSteps(report)
	runID := fmt.Sprint(report["run_id"])
	metrics := tpl.metricColumns()

	type pageLines []string
	var pages []pageLines
	var cur pageLines
	y := 780.0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		pages = append(pages, cur)
		cur = nil
		y = 780
	}
	add := func(size float64, text string) {
		if y < 60 {
			flush()
		}
		cur = append(cur, fmt.Sprintf("BT /F1 %.1f Tf 48 %.1f Td (%s) Tj ET", size, y, pdfEscape(text)))
		y -= size + 6
	}
	add(18, "Open Perf Lab - bench report")
	add(11, fmt.Sprintf("Run: %s", runID))
	add(11, fmt.Sprintf("Status: %s   Scenario: %s", report["status"], report["scenario_id"]))
	add(11, fmt.Sprintf("Generated: %s", report["generated_at"]))
	add(11, fmt.Sprintf("Layout: %s", tpl.label()))
	add(11, fmt.Sprintf("VUs: %g   Samples: %v", report["vus"], report["sample_count"]))

	if tpl.hasWidget("kpis") {
		parts := []string{fmt.Sprintf("Requests: %g", reportKPIValue(report, "requests"))}
		for _, m := range metrics {
			parts = append(parts, fmt.Sprintf("%s: %g", reportMetricLabel(m), reportKPIValue(report, m)))
		}
		add(12, strings.Join(parts, "   "))
	}
	if errMsg := fmt.Sprint(report["error"]); errMsg != "" && errMsg != "<nil>" {
		add(11, "Error: "+errMsg)
	}
	if tpl.hasWidget("steps") {
		add(12, "Per-step stats")
		header := []string{"step"}
		for _, m := range metrics {
			header = append(header, reportMetricLabel(m))
		}
		add(9, strings.Join(append(header, "err"), " | "))
		for _, st := range steps {
			cells := []string{truncatePDF(fmt.Sprint(st["step_name"]), 28)}
			for _, m := range metrics {
				cells = append(cells, fmt.Sprint(st[reportStepMetricKeys[m]]))
			}
			cells = append(cells, fmt.Sprint(st["errors"]))
			add(9, strings.Join(cells, " | "))
		}
	}
	if tpl.hasWidget("errors") {
		br := reportErrorBreakdown(report, steps)
		failing, _ := br["failing_steps"].([]map[string]interface{})
		add(12, fmt.Sprintf("Errors (%d failing step(s))", len(failing)))
		if len(failing) == 0 {
			add(9, "No step recorded an error in the captured samples.")
		}
		for _, f := range failing {
			add(9, fmt.Sprintf("%s | errors %g | rate %g",
				truncatePDF(fmt.Sprint(f["step_name"]), 34), numFrom(f, "errors"), numFrom(f, "error_rate")))
		}
	}
	if tpl.hasWidget("samples") {
		if rows, ok := report["samples"].([]map[string]interface{}); ok && len(rows) > 0 {
			add(12, fmt.Sprintf("Samples (%d shown)", len(rows)))
			for _, s := range rows {
				add(8, fmt.Sprintf("%s | %s | %gms | %g",
					getString(s, "ts"), truncatePDF(getString(s, "step_name"), 20),
					numFrom(s, "latency_ms"), numFrom(s, "status_code")))
			}
		}
	}
	add(8, truncatePDF(fmt.Sprint(report["honesty"]), 90))
	if note := fmt.Sprint(report["template_note"]); note != "" && note != "<nil>" {
		add(8, truncatePDF(note, 90))
	}
	flush()
	if len(pages) == 0 {
		pages = []pageLines{{"BT /F1 12 Tf 48 750 Td (Empty report) Tj ET"}}
	}

	var out bytes.Buffer
	offsets := []int{0}
	writeObj := func(body string) int {
		offsets = append(offsets, out.Len())
		id := len(offsets) - 1
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", id, body)
		return id
	}
	writeStream := func(stream string) int {
		offsets = append(offsets, out.Len())
		id := len(offsets) - 1
		fmt.Fprintf(&out, "%d 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", id, len(stream), stream)
		return id
	}

	out.WriteString("%PDF-1.4\n")
	writeObj("<< /Type /Catalog /Pages 2 0 R >>")
	// Objects: 1 Catalog, 2 Pages, 3 Font, then (content, page)*N. Page object IDs
	// are planned up front so /Kids can be written before the pages themselves.
	n := len(pages)
	contentIDs := make([]int, n)
	pageIDs := make([]int, n)
	next := 4
	for i := 0; i < n; i++ {
		contentIDs[i] = next
		next++
		pageIDs[i] = next
		next++
	}
	var kids strings.Builder
	kids.WriteByte('[')
	for i, id := range pageIDs {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", id)
	}
	kids.WriteByte(']')
	writeObj(fmt.Sprintf("<< /Type /Pages /Kids %s /Count %d >>", kids.String(), n))
	writeObj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i := 0; i < n; i++ {
		_ = writeStream(strings.Join(pages[i], "\n"))
		_ = writeObj(fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			contentIDs[i]))
	}

	xrefPos := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets))
	out.WriteString("0000000000 65535 f \n")
	for i := 1; i < len(offsets); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefPos)
	return out.Bytes()
}

func truncatePDF(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func handlePerfRunBenchPack(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	report, err := buildPerfRunReport(r, runID)
	if err != nil {
		if err.Error() == "not ready" {
			http.Error(w, "not ready", 503)
			return
		}
		http.Error(w, "not found", 404)
		return
	}
	tpl, note := resolveExportTemplate(r, "report")
	applyReportTemplate(report, tpl, note)
	steps := reportSteps(report)
	jsonBytes, _ := json.MarshalIndent(report, "", "  ")
	htmlBytes := renderReportHTML(report, tpl)
	pdfBytes := renderReportPDF(report, tpl)
	csvBytes := reportCSVBytes(steps, tpl)
	manifest := fmt.Sprintf(`OPL bench pack
run_id: %s
scenario_id: %s
status: %s
generated_at: %s
template: %s
files: report.json, report.csv, report.html, report.pdf, MANIFEST.txt
honesty: %s
`, report["run_id"], report["scenario_id"], report["status"], report["generated_at"], tpl.label(), report["honesty"])
	if note != "" {
		manifest += "template_note: " + note + "\n"
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	addZip := func(name string, data []byte) {
		f, e := zw.Create(name)
		if e != nil {
			return
		}
		_, _ = f.Write(data)
	}
	prefix := fmt.Sprintf("opl-bench-%s/", sanitizePerfExportName(runID))
	addZip(prefix+"report.json", jsonBytes)
	addZip(prefix+"report.csv", csvBytes)
	addZip(prefix+"report.html", htmlBytes)
	addZip(prefix+"report.pdf", pdfBytes)
	addZip(prefix+"MANIFEST.txt", []byte(manifest))
	_ = zw.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"opl-bench-%s.zip\"", sanitizePerfExportName(runID)))
	w.Header().Set("X-OPL-Honesty", "Bench pack ZIP: JSON+CSV+HTML+PDF from the same /report payload.")
	w.Header().Set("X-OPL-Template", tpl.label())
	_, _ = w.Write(buf.Bytes())
}

// handlePerfScenarioTrends returns multi-run summary series for trend widgets.
// A trend template supplies the window (runs, SLA threshold) and metric selection.
func handlePerfScenarioTrends(w http.ResponseWriter, r *http.Request, scenarioID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	tpl, note := resolveExportTemplate(r, "trend")
	limit := 25
	if tpl != nil {
		limit = templateWindowInt(tpl.Window, "limit", limit, 1, 100)
		limit = templateWindowInt(tpl.Window, "runs", limit, 1, 100)
	}
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	// Chronological oldest→newest for charting (query newest first then reverse).
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, status, vus, started_at, finished_at, summary_json, error
		FROM `+chTable("load_runs")+` FINAL
		WHERE scenario_id = '%s'%s
		ORDER BY started_at DESC
		LIMIT %d`, escapeSQL(scenarioID), perfOwnedAnd(r), limit))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	metrics := tpl.metricColumns()
	points := make([]map[string]interface{}, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		sum := map[string]interface{}{}
		_ = json.Unmarshal([]byte(getString(row, "summary_json")), &sum)
		p := map[string]interface{}{
			"id":          getString(row, "id"),
			"status":      getString(row, "status"),
			"vus":         getFloat64(row, "vus"),
			"started_at":  getString(row, "started_at"),
			"finished_at": getString(row, "finished_at"),
			"error":       getString(row, "error"),
			// p95 is always present: it drives the SLA breach count and best/worst.
			"p95_ms": numFrom(sum, "p95_ms"),
		}
		for _, m := range metrics {
			if m == "samples" {
				p["samples"] = numFrom(sum, "requests", "samples", "n")
				continue
			}
			p[m] = numFrom(sum, m)
		}
		points = append(points, p)
	}
	// Stats over window
	var bestP95, worstP95 float64
	bestID, worstID := "", ""
	breaches := 0
	slaP95 := 500.0
	if tpl != nil {
		slaP95 = templateWindowFloat(tpl.Window, "sla_p95_ms", slaP95)
	}
	if v := r.URL.Query().Get("sla_p95_ms"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			slaP95 = f
		}
	}
	for i, p := range points {
		p95 := numFrom(p, "p95_ms")
		if i == 0 || (p95 > 0 && (bestP95 == 0 || p95 < bestP95)) {
			bestP95, bestID = p95, fmt.Sprint(p["id"])
		}
		if p95 > worstP95 {
			worstP95, worstID = p95, fmt.Sprint(p["id"])
		}
		if p95 > slaP95 {
			breaches++
		}
		if i > 0 {
			prev := numFrom(points[i-1], "p95_ms")
			p["delta_p95_ms"] = round2(p95 - prev)
		}
	}
	out := map[string]interface{}{
		"ok":           true,
		"scenario_id":  scenarioID,
		"points":       points,
		"count":        len(points),
		"limit":        limit,
		"metrics":      metrics,
		"widgets":      tpl.trendWidgets(),
		"template":     tpl.describe(),
		"sla_p95_ms":   slaP95,
		"best_p95_ms":  bestP95,
		"best_run_id":  bestID,
		"worst_p95_ms": worstP95,
		"worst_run_id": worstID,
		"sla_breaches": breaches,
		"honesty":      "Scenario trend series from load_runs.summary_json (≤limit). A template selects the window, widgets and metrics; it never recomputes a measurement.",
	}
	if note != "" {
		out["template_note"] = note
	}
	writeJSON(w, out)
}
