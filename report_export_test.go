package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderReportPDF(t *testing.T) {
	report := map[string]interface{}{
		"run_id": "run_test_1", "scenario_id": "sc_1", "status": "completed",
		"vus": 10.0, "sample_count": 3, "generated_at": "2026-08-04T12:00:00Z",
		"error": "", "honesty": "unit test",
		"summary": map[string]interface{}{
			"requests": 100.0, "p50_ms": 40.0, "p95_ms": 120.0, "p99_ms": 200.0, "error_rate": 0.01,
		},
		"steps": []map[string]interface{}{
			{"step_name": "Login", "samples": 50, "errors": 0, "error_rate": 0.0, "p50_ms": 30.0, "p95_ms": 90.0, "p99_ms": 110.0, "avg_ms": 40.0, "url": "/login"},
		},
	}
	pdf := renderReportPDF(report, nil)
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) {
		t.Fatalf("missing PDF header")
	}
	if !bytes.Contains(pdf, []byte("%%EOF")) {
		t.Fatalf("missing EOF")
	}
	if !bytes.Contains(pdf, []byte("Open Perf Lab")) {
		t.Fatalf("missing title text")
	}
	if !bytes.Contains(pdf, []byte("/Type /Catalog")) {
		t.Fatalf("missing catalog")
	}
}

func TestRenderReportHTML(t *testing.T) {
	report := map[string]interface{}{
		"run_id": "run_html", "scenario_id": "sc", "status": "completed",
		"generated_at": "now", "honesty": "html test", "error": "",
		"summary": map[string]interface{}{"requests": 10.0, "p95_ms": 50.0},
		"steps": []map[string]interface{}{
			{"step_name": "A", "samples": 1, "errors": 0, "error_rate": 0, "avg_ms": 1, "p50_ms": 1, "p95_ms": 1, "p99_ms": 1, "url": "/"},
		},
	}
	html := string(renderReportHTML(report, nil))
	if !strings.Contains(html, "Open Perf Lab") || !strings.Contains(html, "run_html") {
		t.Fatalf("bad html: %s", html[:min(200, len(html))])
	}
	if !strings.Contains(html, "<table>") {
		t.Fatalf("expected steps table")
	}
}

func TestReportCSVBytes(t *testing.T) {
	b := reportCSVBytes([]map[string]interface{}{
		{"step_name": "X", "samples": 2, "errors": 1, "error_rate": 0.5, "avg_ms": 10, "p50_ms": 9, "p95_ms": 12, "p99_ms": 15, "min_ms": 8, "max_ms": 16, "url": "/x"},
	}, nil)
	s := string(b)
	if !strings.HasPrefix(s, "step_name,") {
		t.Fatalf("header: %q", s)
	}
	if !strings.Contains(s, "X,2,1,0.5") {
		t.Fatalf("row missing: %q", s)
	}
}

func TestNumFrom(t *testing.T) {
	m := map[string]interface{}{"p95_ms": 12.5, "requests": "100"}
	if numFrom(m, "p95_ms") != 12.5 {
		t.Fatal("p95")
	}
	if numFrom(m, "missing", "requests") != 100 {
		t.Fatal("fallback string number")
	}
}
