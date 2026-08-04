package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// buildPerfRunReport loads run + samples and returns the structured bench report map.
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

func writeReportCSV(w http.ResponseWriter, runID string, steps []map[string]interface{}) {
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
}

func reportCSVBytes(steps []map[string]interface{}) []byte {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
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

func renderReportHTML(report map[string]interface{}) []byte {
	sum := summaryMap(report)
	steps := reportSteps(report)
	runID := fmt.Sprint(report["run_id"])
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
.honesty{margin-top:24px;font-size:11px;color:#666;border-top:1px solid #eee;padding-top:12px}
@media print{body{margin:12mm}}
</style></head><body>`)
	b.WriteString(`<h1>Open Perf Lab — bench report</h1>`)
	b.WriteString(`<div class="meta">Run <strong>` + esc(runID) + `</strong> · status ` + esc(fmt.Sprint(report["status"])) +
		` · scenario ` + esc(fmt.Sprint(report["scenario_id"])) +
		` · generated ` + esc(fmt.Sprint(report["generated_at"])) + `</div>`)
	b.WriteString(`<div class="kpis">`)
	kpis := []struct{ l, k string }{
		{"Requests", "requests"}, {"p50 ms", "p50_ms"}, {"p95 ms", "p95_ms"}, {"p99 ms", "p99_ms"}, {"Error rate", "error_rate"},
	}
	for _, k := range kpis {
		v := numFrom(sum, k.k, "samples", "n")
		if k.k == "requests" {
			v = numFrom(sum, "requests", "samples", "n")
		}
		b.WriteString(`<div class="kpi"><div class="l">` + esc(k.l) + `</div><div class="v">` + esc(fmt.Sprintf("%g", v)) + `</div></div>`)
	}
	b.WriteString(`</div>`)
	if errMsg := fmt.Sprint(report["error"]); errMsg != "" && errMsg != "<nil>" {
		b.WriteString(`<p><strong>Error:</strong> ` + esc(errMsg) + `</p>`)
	}
	b.WriteString(`<h2 style="font-size:16px">Per-step stats</h2><table><thead><tr>`)
	for _, h := range []string{"Step", "N", "Errors", "Err%", "avg", "p50", "p95", "p99", "URL"} {
		b.WriteString(`<th>` + h + `</th>`)
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, st := range steps {
		b.WriteString(`<tr>`)
		b.WriteString(`<td>` + esc(fmt.Sprint(st["step_name"])) + `</td>`)
		for _, k := range []string{"samples", "errors", "error_rate", "avg_ms", "p50_ms", "p95_ms", "p99_ms"} {
			b.WriteString(`<td class="n">` + esc(fmt.Sprint(st[k])) + `</td>`)
		}
		b.WriteString(`<td>` + esc(fmt.Sprint(st["url"])) + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<p class="honesty">` + esc(fmt.Sprint(report["honesty"])) + `</p>`)
	b.WriteString(`</body></html>`)
	return []byte(b.String())
}

// --- Minimal PDF 1.4 writer (Helvetica, multi-page text tables) ---

func pdfEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	return s
}

func renderReportPDF(report map[string]interface{}) []byte {
	sum := summaryMap(report)
	steps := reportSteps(report)
	runID := fmt.Sprint(report["run_id"])

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
	add(11, fmt.Sprintf("VUs: %g   Samples: %v", report["vus"], report["sample_count"]))
	add(12, fmt.Sprintf("Requests: %g   p50: %g   p95: %g   p99: %g   err: %g",
		numFrom(sum, "requests", "samples", "n"),
		numFrom(sum, "p50_ms"), numFrom(sum, "p95_ms"), numFrom(sum, "p99_ms"), numFrom(sum, "error_rate")))
	if errMsg := fmt.Sprint(report["error"]); errMsg != "" && errMsg != "<nil>" {
		add(11, "Error: "+errMsg)
	}
	add(12, "Per-step stats")
	add(9, "step | n | err | p50 | p95 | p99")
	for _, st := range steps {
		row := fmt.Sprintf("%s | %v | %v | %v | %v | %v",
			truncatePDF(fmt.Sprint(st["step_name"]), 28),
			st["samples"], st["error_rate"], st["p50_ms"], st["p95_ms"], st["p99_ms"])
		add(9, row)
	}
	add(8, truncatePDF(fmt.Sprint(report["honesty"]), 90))
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
	// Pages dict written after we know kids — reserve by writing placeholder then... 
	// Instead: compute page object IDs first.
	// Objects: 1 Catalog, 2 Pages, 3 Font, then (content, page)*N
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
		stream := strings.Join(pages[i], "\n")
		cid := writeStream(stream)
		if cid != contentIDs[i] {
			// Sequential write must match planned IDs.
		}
		pid := writeObj(fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			contentIDs[i]))
		_ = pid
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
	steps := reportSteps(report)
	jsonBytes, _ := json.MarshalIndent(report, "", "  ")
	htmlBytes := renderReportHTML(report)
	pdfBytes := renderReportPDF(report)
	csvBytes := reportCSVBytes(steps)
	manifest := fmt.Sprintf(`OPL bench pack
run_id: %s
scenario_id: %s
status: %s
generated_at: %s
files: report.json, report.csv, report.html, report.pdf, MANIFEST.txt
honesty: %s
`, report["run_id"], report["scenario_id"], report["status"], report["generated_at"], report["honesty"])

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
	_, _ = w.Write(buf.Bytes())
}

// handlePerfScenarioTrends returns multi-run summary series for trend widgets.
func handlePerfScenarioTrends(w http.ResponseWriter, r *http.Request, scenarioID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	limit := 25
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
	points := make([]map[string]interface{}, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		sum := map[string]interface{}{}
		_ = json.Unmarshal([]byte(getString(row, "summary_json")), &sum)
		p95 := numFrom(sum, "p95_ms")
		p50 := numFrom(sum, "p50_ms")
		p99 := numFrom(sum, "p99_ms")
		errRate := numFrom(sum, "error_rate")
		n := numFrom(sum, "requests", "samples", "n")
		points = append(points, map[string]interface{}{
			"id":          getString(row, "id"),
			"status":      getString(row, "status"),
			"vus":         getFloat64(row, "vus"),
			"started_at":  getString(row, "started_at"),
			"finished_at": getString(row, "finished_at"),
			"p50_ms":      p50,
			"p95_ms":      p95,
			"p99_ms":      p99,
			"error_rate":  errRate,
			"samples":     n,
			"error":       getString(row, "error"),
		})
	}
	// Stats over window
	var bestP95, worstP95 float64
	bestID, worstID := "", ""
	breaches := 0
	slaP95 := 500.0
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
	writeJSON(w, map[string]interface{}{
		"ok":           true,
		"scenario_id":  scenarioID,
		"points":       points,
		"count":        len(points),
		"sla_p95_ms":   slaP95,
		"best_p95_ms":  bestP95,
		"best_run_id":  bestID,
		"worst_p95_ms": worstP95,
		"worst_run_id": worstID,
		"sla_breaches": breaches,
		"honesty":      "Scenario trend series from load_runs.summary_json (≤limit). Not a full template report builder.",
	})
}
