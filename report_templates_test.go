package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeTemplateKind(t *testing.T) {
	for in, want := range map[string]string{
		"":       "report",
		"report": "report",
		"trend":  "trend",
		"Trends": "trend",
		"bogus":  "report",
	} {
		if got := normalizeTemplateKind(in); got != want {
			t.Fatalf("%q => %q want %q", in, got, want)
		}
	}
}

func TestFilterKnownDropsUnknownAndKeepsCanonicalOrder(t *testing.T) {
	got := filterKnown([]string{"error_rate", "bogus", "p50_ms"}, reportTemplateMetrics)
	if strings.Join(got, ",") != "p50_ms,error_rate" {
		t.Fatalf("got %v", got)
	}
	if len(filterKnown([]string{"nope"}, reportTemplateWidgets["trend"])) != 0 {
		t.Fatal("unknown widget must be dropped")
	}
}

func TestTemplateWindowIntClamps(t *testing.T) {
	win := map[string]interface{}{"limit": 500.0, "runs": "7", "bad": "x"}
	if got := templateWindowInt(win, "limit", 25, 1, 100); got != 100 {
		t.Fatalf("clamp high => %d", got)
	}
	if got := templateWindowInt(win, "runs", 25, 1, 100); got != 7 {
		t.Fatalf("string number => %d", got)
	}
	if got := templateWindowInt(win, "bad", 25, 1, 100); got != 25 {
		t.Fatalf("unparsable => %d", got)
	}
	if got := templateWindowInt(nil, "limit", 25, 1, 100); got != 25 {
		t.Fatalf("nil window => %d", got)
	}
}

func TestNilTemplateBehavesAsFullLayout(t *testing.T) {
	var tpl *reportTemplate
	if !tpl.hasWidget("anything") || !tpl.hasMetric("anything") {
		t.Fatal("nil template must allow every widget/metric")
	}
	if got := tpl.label(); got != "full layout (no template)" {
		t.Fatalf("label=%q", got)
	}
	if tpl.describe() != nil {
		t.Fatal("nil template describes as nil")
	}
	if len(tpl.metricColumns()) != len(reportTemplateMetrics) {
		t.Fatal("nil template exposes every metric")
	}
	if len(tpl.trendWidgets()) != len(reportTemplateWidgets["trend"]) {
		t.Fatal("nil template exposes every trend widget")
	}
}

func sampleReport() map[string]interface{} {
	return map[string]interface{}{
		"run_id": "run-t", "scenario_id": "scn-t", "status": "failed",
		"vus": 5.0, "sample_count": 2, "generated_at": "2026-08-04T00:00:00Z",
		"error": "sla breach", "honesty": "base honesty.",
		"summary": map[string]interface{}{
			"requests": 100.0, "p50_ms": 10.0, "p95_ms": 90.0, "p99_ms": 150.0, "avg_ms": 20.0, "error_rate": 0.25,
		},
		"steps": []map[string]interface{}{
			{"step_name": "Login", "samples": 60, "errors": 15, "error_rate": 0.25, "avg_ms": 20.0, "p50_ms": 10.0, "p95_ms": 90.0, "p99_ms": 150.0, "min_ms": 5.0, "max_ms": 200.0, "url": "/login"},
			{"step_name": "Search", "samples": 40, "errors": 0, "error_rate": 0.0, "avg_ms": 12.0, "p50_ms": 8.0, "p95_ms": 30.0, "p99_ms": 40.0, "min_ms": 4.0, "max_ms": 60.0, "url": "/search"},
		},
		"_samples": []map[string]interface{}{
			{"ts": "2026-08-04 00:00:00.000", "step_name": "Login", "latency_ms": 12.0, "status_code": 200.0, "ok": 1.0, "url": "/login"},
			{"ts": "2026-08-04 00:00:01.000", "step_name": "Login", "latency_ms": 90.0, "status_code": 500.0, "ok": 0.0, "url": "/login"},
		},
	}
}

func TestApplyReportTemplateTrimsWidgetsAndMetrics(t *testing.T) {
	report := sampleReport()
	tpl := &reportTemplate{
		ID: "rtpl-1", Name: "Exec summary", Kind: "report",
		Widgets: []string{"kpis", "errors"},
		Metrics: []string{"p95_ms", "error_rate"},
	}
	applyReportTemplate(report, tpl, "")
	if _, ok := report["_samples"]; ok {
		t.Fatal("_samples must never reach the payload")
	}
	if _, ok := report["steps"]; ok {
		t.Fatal("steps widget not selected — must be dropped")
	}
	if _, ok := report["summary"]; ok {
		t.Fatal("summary widget not selected — must be dropped")
	}
	if _, ok := report["samples"]; ok {
		t.Fatal("samples widget not selected — must be absent")
	}
	kpis, ok := report["kpis"].(map[string]interface{})
	if !ok {
		t.Fatalf("kpis missing: %#v", report["kpis"])
	}
	if _, has := kpis["p50_ms"]; has {
		t.Fatal("p50_ms not selected — must not appear in kpis")
	}
	if numFrom(kpis, "p95_ms") != 90 || numFrom(kpis, "error_rate") != 0.25 {
		t.Fatalf("kpis=%#v", kpis)
	}
	errs, ok := report["errors"].(map[string]interface{})
	if !ok {
		t.Fatalf("errors block missing")
	}
	failing, _ := errs["failing_steps"].([]map[string]interface{})
	if len(failing) != 1 || failing[0]["step_name"] != "Login" {
		t.Fatalf("failing=%#v", failing)
	}
	if errs["run_error"] != "sla breach" {
		t.Fatalf("run_error=%v", errs["run_error"])
	}
	if desc, _ := report["template"].(map[string]interface{}); desc == nil || desc["name"] != "Exec summary" {
		t.Fatalf("template block=%#v", report["template"])
	}
	if !strings.Contains(report["honesty"].(string), "Exec summary") {
		t.Fatalf("honesty must name the layout: %v", report["honesty"])
	}
}

func TestApplyReportTemplateSamplesCap(t *testing.T) {
	report := sampleReport()
	tpl := &reportTemplate{
		Name: "With samples", Kind: "report",
		Widgets: []string{"samples"},
		Metrics: []string{"p95_ms"},
		Options: map[string]interface{}{"sample_cap": 1.0},
	}
	applyReportTemplate(report, tpl, "")
	rows, ok := report["samples"].([]map[string]interface{})
	if !ok || len(rows) != 1 {
		t.Fatalf("samples=%#v", report["samples"])
	}
	if report["samples_capped_at"] != 1 {
		t.Fatalf("cap=%v", report["samples_capped_at"])
	}
}

func TestApplyReportTemplateNilKeepsFullLayout(t *testing.T) {
	report := sampleReport()
	applyReportTemplate(report, nil, `template "missing" not found in this scope — rendered the full layout instead`)
	if _, ok := report["steps"]; !ok {
		t.Fatal("full layout keeps steps")
	}
	if _, ok := report["summary"]; !ok {
		t.Fatal("full layout keeps summary")
	}
	if report["template"] != nil {
		t.Fatal("no template applied")
	}
	note, _ := report["template_note"].(string)
	if !strings.Contains(note, "not found") {
		t.Fatalf("a missing template must be reported plainly, got %q", note)
	}
}

func TestFilterStepMetricsKeepsIdentityColumns(t *testing.T) {
	steps := reportSteps(sampleReport())
	out := filterStepMetrics(steps, []string{"p95_ms"})
	if len(out) != 2 {
		t.Fatalf("rows=%d", len(out))
	}
	row := out[0]
	for _, must := range []string{"step_name", "url", "errors", "p95_ms"} {
		if _, ok := row[must]; !ok {
			t.Fatalf("missing %q in %#v", must, row)
		}
	}
	for _, gone := range []string{"p50_ms", "p99_ms", "avg_ms", "min_ms", "max_ms"} {
		if _, ok := row[gone]; ok {
			t.Fatalf("%q should have been filtered out", gone)
		}
	}
}

func TestReportCSVColumnsFollowTemplate(t *testing.T) {
	full := reportCSVColumns(nil)
	if full[0] != "step_name" || len(full) != 11 {
		t.Fatalf("full header=%v", full)
	}
	tpl := &reportTemplate{Kind: "report", Metrics: []string{"error_rate", "p95_ms"}}
	got := strings.Join(reportCSVColumns(tpl), ",")
	if got != "step_name,p95_ms,error_rate,errors,url" {
		t.Fatalf("template header=%q", got)
	}
}

func TestRenderReportWithTemplateOmitsUnselectedSections(t *testing.T) {
	report := sampleReport()
	tpl := &reportTemplate{
		Name: "KPIs only", Kind: "report",
		Widgets: []string{"kpis"},
		Metrics: []string{"p95_ms"},
	}
	applyReportTemplate(report, tpl, "")
	out := string(renderReportHTML(report, tpl))
	if !strings.Contains(out, "KPIs only") {
		t.Fatal("HTML must name the layout used")
	}
	if strings.Contains(out, "Per-step stats") {
		t.Fatal("steps widget not selected — table must be absent")
	}
	if strings.Contains(out, "p50 ms") {
		t.Fatal("unselected metric must not appear")
	}
	pdf := renderReportPDF(report, tpl)
	if !strings.Contains(string(pdf), "Layout: KPIs only") {
		t.Fatal("PDF must name the layout used")
	}
}

func TestReportTemplateRowToJSONValidates(t *testing.T) {
	widgets, _ := json.Marshal([]string{"latency_band", "kpis", "not_a_widget"})
	metrics, _ := json.Marshal([]string{"p95_ms", "nope"})
	row := map[string]interface{}{
		"id": "rtpl-x", "name": "Weekly", "kind": "trend",
		"widgets_json": string(widgets), "metrics_json": string(metrics),
		"window_json": `{"limit":10,"sla_p95_ms":250}`, "options_json": `{}`,
	}
	got := reportTemplateRowToJSON(row)
	if strings.Join(got["widgets"].([]string), ",") != "kpis,latency_band" {
		t.Fatalf("widgets=%v", got["widgets"])
	}
	if strings.Join(got["metrics"].([]string), ",") != "p95_ms" {
		t.Fatalf("metrics=%v", got["metrics"])
	}
	win := got["window"].(map[string]interface{})
	if templateWindowInt(win, "limit", 25, 1, 100) != 10 {
		t.Fatalf("window=%v", win)
	}
}
