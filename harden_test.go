package main

import (
	"strings"
	"testing"
)

func TestInitialLoadRunStatus(t *testing.T) {
	st, err := initialLoadRunStatus(false, map[string]interface{}{"dispatched": false})
	if st != "created" || err != "" {
		t.Fatalf("no dispatch: got %q %q", st, err)
	}
	st, err = initialLoadRunStatus(true, map[string]interface{}{"dispatched": true})
	if st != "running" || err != "" {
		t.Fatalf("dispatched: got %q %q", st, err)
	}
	st, err = initialLoadRunStatus(true, map[string]interface{}{"dispatched": false, "error": "docker down"})
	if st != "failed" || err != "docker down" {
		t.Fatalf("dispatch error: got %q %q", st, err)
	}
	st, err = initialLoadRunStatus(true, map[string]interface{}{"dispatched": false})
	if st != "failed" || err == "" {
		t.Fatalf("dispatch silent fail: got %q %q", st, err)
	}
}

func TestRunStatusTerminal(t *testing.T) {
	for _, s := range []string{"passed", "failed", "completed", "cancelled", "aborted"} {
		if !runStatusTerminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []string{"running", "created", ""} {
		if runStatusTerminal(s) {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}

func TestEvaluateSLAFailClosed(t *testing.T) {
	pass, reasons := evaluateSLAFailClosed(map[string]interface{}{
		"requests": 10, "p95_ms": 80.0, "error_rate": 0.0,
	}, map[string]interface{}{"p95_ms": 500.0, "error_rate_max": 0.05})
	if !pass || len(reasons) != 0 {
		t.Fatalf("expected pass, got %v %v", pass, reasons)
	}
	pass, reasons = evaluateSLAFailClosed(map[string]interface{}{
		"requests": 10, "p95_ms": 900.0, "error_rate": 0.0,
	}, map[string]interface{}{"p95_ms": 500.0, "error_rate_max": 0.05})
	if pass || len(reasons) == 0 {
		t.Fatalf("expected fail on p95, got %v %v", pass, reasons)
	}
}

func TestResolveLoadPolicy(t *testing.T) {
	p, sched, _ := resolveLoadPolicy("smooth", "", nil)
	if p != "ramp" {
		t.Fatalf("smooth -> ramp, got %q", p)
	}
	if sched["policy"] != "smooth" {
		t.Fatalf("policy id missing: %v", sched)
	}
	p, _, _ = resolveLoadPolicy("sustained", "", nil)
	if p != "soak" {
		t.Fatalf("sustained -> soak, got %q", p)
	}
	p, _, _ = resolveLoadPolicy("stress", "", nil)
	if p != "spike" {
		t.Fatalf("stress -> spike, got %q", p)
	}
}

func TestAggregateRunSteps(t *testing.T) {
	steps := aggregateRunSteps([]map[string]interface{}{
		{"step_name": "Login", "latency_ms": 100.0, "ok": true, "status_code": 200.0, "url": "/login"},
		{"step_name": "Login", "latency_ms": 200.0, "ok": false, "status_code": 500.0, "url": "/login"},
		{"step_name": "Home", "latency_ms": 50.0, "ok": 1, "status_code": 200.0, "url": "/"},
	})
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	login := steps[0]
	if login["step_name"] != "Login" || login["samples"].(int) != 2 {
		t.Fatalf("login agg: %#v", login)
	}
	if login["errors"].(int) != 1 {
		t.Fatalf("login errors: %#v", login)
	}
}

func TestPerfInstrumentationHonesty(t *testing.T) {
	ok, msg := perfInstrumentationHonesty("https://example.com/")
	if ok || !strings.Contains(msg, "not OPA-instrumented") {
		t.Fatalf("example.com: %v %q", ok, msg)
	}
	ok, msg = perfInstrumentationHonesty("http://node-app:3000/hello")
	if !ok || !strings.Contains(msg, "compose demo") {
		t.Fatalf("node-app: %v %q", ok, msg)
	}
}

func TestParseDockerInspectLine(t *testing.T) {
	snap := parseDockerInspectLine("opa-jmeter-x-w0", "abcdef0123456789|running|true|2026-01-01T00:00:00Z||0|justb4/jmeter:5.5")
	if !snap["found"].(bool) || snap["status"] != "running" || snap["running"] != true {
		t.Fatalf("%#v", snap)
	}
	if snap["id"] != "abcdef012345" {
		t.Fatalf("id truncate: %v", snap["id"])
	}
}

func TestContainerNamesFromAny(t *testing.T) {
	if got := containerNamesFromAny([]string{"a", "b"}); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if got := containerNamesFromAny([]interface{}{"a", 1, "b"}); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

