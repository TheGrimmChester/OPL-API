package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

// pinPerfDNS pins hostname→IP resolution for the duration of the test so the SSRF
// guard can be exercised with no DNS at all (the OPA Checkup sandbox runs on an
// internal network with an egress allowlist and no general DNS). Hosts absent from
// the map resolve as NXDOMAIN. This stubs only the lookup — ipBlockedForPerf still
// decides what is allowed, so a pinned private address is still rejected.
//
// 192.0.2.0/24 (RFC 5737 TEST-NET-1) is the stand-in for "a public address": it is
// not loopback/private/link-local, so it survives the guard, and it is reserved for
// documentation so nothing ever dials it for real.
func pinPerfDNS(t *testing.T, hosts map[string]string) {
	t.Helper()
	pinned := make(map[string][]net.IP, len(hosts))
	for host, addr := range hosts {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("pinPerfDNS: %q is not a valid IP (host %q)", addr, host)
		}
		pinned[strings.ToLower(strings.TrimSpace(host))] = []net.IP{ip}
	}
	prev := perfLookupIP
	t.Cleanup(func() { perfLookupIP = prev })
	perfLookupIP = func(host string) ([]net.IP, error) {
		if ips, ok := pinned[strings.ToLower(strings.TrimSpace(host))]; ok {
			return ips, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

// TestResolveAllowedPerfHostPinned locks in that pinning the resolver does not soften
// the guard: a host resolving into a private range is still refused. If someone ever
// "fixes" an offline test by loosening ipBlockedForPerf, this fails.
func TestResolveAllowedPerfHostPinned(t *testing.T) {
	pinPerfDNS(t, map[string]string{
		"public.test":   "192.0.2.10",
		"internal.test": "10.1.2.3",
		"metadata.test": "169.254.169.254",
	})

	ips, err := resolveAllowedPerfHost("public.test")
	if err != nil || len(ips) != 1 || ips[0].String() != "192.0.2.10" {
		t.Fatalf("public host: ips=%v err=%v", ips, err)
	}
	for _, host := range []string{"internal.test", "metadata.test"} {
		if _, err := resolveAllowedPerfHost(host); err == nil {
			t.Fatalf("%s: expected private/link-local rejection, got nil", host)
		}
	}
	if _, err := resolveAllowedPerfHost("unpinned.test"); err == nil {
		t.Fatal("unresolvable host: expected error, got nil")
	}
}

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

func TestEvaluateSLAFailsWhenRPSBelowMin(t *testing.T) {
	pass, reasons := evaluateSLAFailClosed(map[string]interface{}{
		"requests": 10, "p95_ms": 80.0, "error_rate": 0.0, "rps": 5.0,
	}, map[string]interface{}{"p95_ms": 500.0, "error_rate_max": 0.05, "rps_min": 10.0})
	if pass || len(reasons) == 0 {
		t.Fatalf("expected fail on rps_min, got %v %v", pass, reasons)
	}
	joined := fmt.Sprintf("%v", reasons)
	if !strings.Contains(joined, "rps") {
		t.Fatalf("reason should mention rps, got %v", reasons)
	}
	pass, reasons = evaluateSLAFailClosed(map[string]interface{}{
		"requests": 10, "p95_ms": 80.0, "error_rate": 0.0, "rps": 12.0,
	}, map[string]interface{}{"rps_min": 10.0})
	if !pass || len(reasons) != 0 {
		t.Fatalf("expected pass when rps >= rps_min, got %v %v", pass, reasons)
	}
	pass, reasons = evaluateSLAFailClosed(map[string]interface{}{
		"requests": 10, "p95_ms": 80.0, "error_rate": 0.0,
	}, map[string]interface{}{"rps_min": 1.0})
	if pass || len(reasons) == 0 {
		t.Fatalf("expected fail when rps missing, got %v %v", pass, reasons)
	}
	joined = fmt.Sprintf("%v", reasons)
	if !strings.Contains(joined, "missing rps") {
		t.Fatalf("want missing rps, got %v", reasons)
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

