package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// JMeter Perf Lab hardening: auth, tenancy, SSRF (dial pinning), caps, unsafe JMX, fail-closed SLA.

func perfCallerIsAdmin(r *http.Request) bool {
	if !authEnforced {
		return true
	}
	return hasPermission(r.Header.Get("X-User-Role"), "admin")
}

func perfRequireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if perfCallerIsAdmin(r) {
		return true
	}
	http.Error(w, "admin required", 403)
	return false
}

// perfAllowMetricsWrite requires admin or OPA_PERF_RUNNER_TOKEN (federation-style header).
func perfAllowMetricsWrite(r *http.Request) bool {
	if perfCallerIsAdmin(r) {
		return true
	}
	want := strings.TrimSpace(envOr("OPA_PERF_RUNNER_TOKEN", ""))
	if want == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-OPA-Perf-Runner-Token"))
	if got == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	return got != "" && got == want
}

// perfOwnedAnd scopes by-id reads to the write tenant when auth is on.
func perfOwnedAnd(r *http.Request) string {
	if !authEnforced || queryClient == nil {
		return ""
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	return " AND " + ctx.OwnedRowPredicate("")
}

func clampPerfVUs(v int) int {
	max := 100
	if n, err := strconv.Atoi(strings.TrimSpace(envOr("OPA_PERF_MAX_VUS", "100"))); err == nil && n > 0 {
		max = n
	}
	if v <= 0 {
		return 1
	}
	if v > max {
		return max
	}
	return v
}

func clampPerfDuration(sec int) int {
	max := 600
	if n, err := strconv.Atoi(strings.TrimSpace(envOr("OPA_PERF_MAX_DURATION", "600"))); err == nil && n > 0 {
		max = n
	}
	if sec <= 0 {
		return 30
	}
	if sec > max {
		return max
	}
	return sec
}

var unsafeJMXNeedles = []string{
	"BeanShellSampler", "BeanShellPreProcessor", "BeanShellPostProcessor", "BeanShellAssertion",
	"JSR223Sampler", "JSR223PreProcessor", "JSR223PostProcessor", "JSR223Assertion",
	"BSFSampler", "BSFPreProcessor", "BSFPostProcessor", "BSFAssertion",
	"SystemSampler", "OSProcessSampler", "JavaSampler", "JUnitSampler",
	"JDBCSampler", "FTPSampler", "SMTPSampler", "TCPSampler", "LDAPSampler",
	"MailReaderSampler", "JMSSampler", "AccessLogSampler",
	"PublisherSampler", "SubscriberSampler",
}

func jmxContainsUnsafeElements(jmx string) bool {
	if strings.EqualFold(strings.TrimSpace(envOr("OPA_PERF_ALLOW_UNSAFE_JMX", "")), "1") {
		return false
	}
	for _, n := range unsafeJMXNeedles {
		if strings.Contains(jmx, n) {
			return true
		}
	}
	return false
}

func safeCompileRegex(expr string) (*regexp.Regexp, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty regex")
	}
	if len(expr) > 256 {
		return nil, fmt.Errorf("regex too long")
	}
	if strings.Contains(expr, "++") || strings.Contains(expr, "**") {
		return nil, fmt.Errorf("regex rejected")
	}
	return regexp.Compile(expr)
}

// isWeirdPerfHostForm rejects decimal/hex/octal IP encodings that bypass literal checks.
func isWeirdPerfHostForm(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return true
	}
	if strings.HasPrefix(h, "0x") {
		return true
	}
	// Pure decimal IPv4 (e.g. 2130706433 → 127.0.0.1)
	if _, err := strconv.ParseUint(h, 10, 32); err == nil {
		return true
	}
	// Dotted forms with hex/octal segments (0x7f.0.0.1, 0177.0.0.1)
	parts := strings.Split(h, ".")
	if len(parts) >= 2 && len(parts) <= 4 {
		weird := false
		for _, p := range parts {
			if p == "" {
				return true
			}
			if strings.HasPrefix(p, "0x") {
				weird = true
				continue
			}
			if len(p) > 1 && p[0] == '0' && isAllDigits(p) {
				weird = true
				continue
			}
			if !isAllDigits(p) && !strings.ContainsAny(p, "abcdef") {
				return false // hostname-ish
			}
			if strings.ContainsAny(p, "abcdef") && !strings.HasPrefix(p, "0x") {
				// bare hex segment without 0x — still suspicious in dotted IP
				if isAllHex(p) && !isAllDigits(p) {
					weird = true
				}
			}
		}
		if weird {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isAllHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

func ipBlockedForPerf(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 already link-local; explicit metadata
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

// perfInternalHostAllowed returns true when host is listed in OPA_PERF_INTERNAL_HOSTS
// (comma-separated). Used for local compose targets like node-app that resolve to
// RFC1918 addresses but are intentionally load-tested for APM correlation.
func perfInternalHostAllowed(host string) bool {
	allow := strings.TrimSpace(envOr("OPA_PERF_INTERNAL_HOSTS", ""))
	if allow == "" {
		return false
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, part := range strings.Split(allow, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" && p == h {
			return true
		}
	}
	return false
}

// resolveAllowedPerfHost looks up host and returns only non-blocked IPs (for dial pinning).
func resolveAllowedPerfHost(host string) ([]net.IP, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("missing host")
	}
	lower := strings.ToLower(host)
	internalOK := perfInternalHostAllowed(host)
	if !internalOK && (lower == "localhost" || lower == "metadata.google.internal" ||
		strings.HasSuffix(lower, ".internal") || lower == "host.docker.internal") {
		return nil, fmt.Errorf("host not allowed")
	}
	if isWeirdPerfHostForm(host) {
		return nil, fmt.Errorf("encoded/numeric host not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ipBlockedForPerf(ip) && !internalOK {
			return nil, fmt.Errorf("private/link-local address not allowed")
		}
		return []net.IP{ip}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup failed")
	}
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ipBlockedForPerf(ip) {
			if internalOK {
				allowed = append(allowed, ip)
			}
			continue
		}
		allowed = append(allowed, ip)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("private/link-local address not allowed")
	}
	return allowed, nil
}

func isBlockedPerfURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	_, err = resolveAllowedPerfHost(host)
	return err
}

// perfScenarioHTTPURLsBlocked checks target_url + http step URLs (generated/simple scenarios).
func perfScenarioHTTPURLsBlocked(sc map[string]interface{}) error {
	if sc == nil {
		return fmt.Errorf("scenario missing")
	}
	if tu := strings.TrimSpace(getString(sc, "target_url")); tu != "" {
		if err := isBlockedPerfURL(tu); err != nil {
			return fmt.Errorf("target_url: %w", err)
		}
	}
	for _, step := range scenarioSteps(sc) {
		typ, _ := step["type"].(string)
		if typ == "" {
			typ = "http"
		}
		if typ != "http" {
			continue
		}
		u := strings.TrimSpace(fmt.Sprint(step["url"]))
		if u == "" || u == "<nil>" {
			continue
		}
		// Skip unresolved template URLs; validate literal http(s) targets.
		if strings.Contains(u, "${") || strings.Contains(u, "{{") {
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			if err := isBlockedPerfURL(u); err != nil {
				return fmt.Errorf("step url: %w", err)
			}
		}
	}
	return nil
}

func perfPinnedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := resolveAllowedPerfHost(host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	d := net.Dialer{Timeout: 5 * time.Second}
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no dialable address")
	}
	return nil, lastErr
}

func perfValidateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           perfPinnedDialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func perfPeerLoadHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: perfPinnedDialContext,
		},
	}
}

// evaluateSLAFailClosed fails when SLA metrics are missing, requests==0, or SLA empty
// (unless OPA_PERF_ALLOW_EMPTY_SLA=1). Callers should pass thresholds when sla_json is empty.
func evaluateSLAFailClosed(summary map[string]interface{}, sla map[string]interface{}) (pass bool, reasons []string) {
	if summary == nil {
		summary = map[string]interface{}{}
	}
	if n, ok := asFloat(summary["requests"]); !ok || n <= 0 {
		return false, []string{"no requests in summary"}
	}
	if sla == nil || len(sla) == 0 {
		if strings.EqualFold(strings.TrimSpace(envOr("OPA_PERF_ALLOW_EMPTY_SLA", "")), "1") {
			return true, nil
		}
		return false, []string{"empty sla (fail closed)"}
	}
	pass = true
	if _, want := asFloat(sla["p95_ms"]); want {
		p95, ok2 := asFloat(summary["p95_ms"])
		if !ok2 {
			pass = false
			reasons = append(reasons, "missing p95_ms")
		} else if max, _ := asFloat(sla["p95_ms"]); p95 > max {
			pass = false
			reasons = append(reasons, fmt.Sprintf("p95_ms %.1f > %.1f", p95, max))
		}
	}
	if _, want := asFloat(sla["error_rate_max"]); want {
		er, ok2 := asFloat(summary["error_rate"])
		if !ok2 {
			pass = false
			reasons = append(reasons, "missing error_rate")
		} else if max, _ := asFloat(sla["error_rate_max"]); er > max {
			pass = false
			reasons = append(reasons, fmt.Sprintf("error_rate %.4f > %.4f", er, max))
		}
	}
	if _, want := asFloat(sla["rps_min"]); want {
		rps, ok2 := asFloat(summary["rps"])
		if !ok2 {
			pass = false
			reasons = append(reasons, "missing rps")
		} else if min, _ := asFloat(sla["rps_min"]); rps < min {
			pass = false
			reasons = append(reasons, fmt.Sprintf("rps %.2f < %.2f", rps, min))
		}
	}
	return pass, reasons
}

func runStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "passed", "failed", "error", "cancelled", "canceled", "aborted":
		return true
	default:
		return false
	}
}

// initialLoadRunStatus chooses the ClickHouse status after POST /api/perf/runs.
// Undispatched runs must not stay "running" forever; failed dispatch is terminal "failed".
func initialLoadRunStatus(wantDispatch bool, dispatchInfo map[string]interface{}) (status, errMsg string) {
	if !wantDispatch {
		return "created", ""
	}
	if dispatchInfo != nil {
		if ok, _ := dispatchInfo["dispatched"].(bool); ok {
			return "running", ""
		}
		if fb, ok := dispatchInfo["node_fallback"].(map[string]interface{}); ok {
			if ok2, _ := fb["dispatched"].(bool); ok2 {
				return "running", ""
			}
			if e, _ := fb["error"].(string); strings.TrimSpace(e) != "" {
				return "failed", strings.TrimSpace(e)
			}
		}
		if e, _ := dispatchInfo["error"].(string); strings.TrimSpace(e) != "" {
			return "failed", strings.TrimSpace(e)
		}
	}
	return "failed", "dispatch did not start an engine"
}

func httpStatusOK2xx(code int) bool {
	return code >= 200 && code < 300
}

// neverInstrumentedHosts never emit OPA spans / load_run_id tags — Open traces stay empty.
var neverInstrumentedHosts = []string{
	"example.com", "example.org", "example.net",
	"httpbin.org", "httpbingo.org", "postman-echo.com",
	"jsonplaceholder.typicode.com", "reqres.in",
	"google.com", "www.google.com", "cloudflare.com",
}

// likelyInstrumentedHosts are compose-demo service names that ship with OPA instrumentation.
var likelyInstrumentedHosts = []string{
	"node-app", "java-app", "python-app", "dotnet-app", "go-app", "php-app",
}

// perfInstrumentationHonesty explains whether Open traces / load_run_id correlation can work.
// Public demo hosts and uninstrumented apps never yield spans — do not imply otherwise.
func perfInstrumentationHonesty(targetURL string) (likelyInstrumented bool, honesty string) {
	raw := strings.TrimSpace(strings.ToLower(targetURL))
	if raw == "" {
		return false, "No target_url — OPA correlation needs an instrumented app that records X-OPA-Load-Run-Id / baggage load_run_id on spans."
	}
	host := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host = strings.ToLower(u.Hostname())
	} else {
		// bare host:port/path
		host = strings.Split(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/")[0]
		host = strings.Split(host, ":")[0]
	}
	for _, bad := range neverInstrumentedHosts {
		if host == bad || strings.HasSuffix(host, "."+bad) {
			return false, "Target host is not OPA-instrumented — Open traces filtered by load_run_id will be empty. Use an instrumented service (compose default: http://node-app:3000/hello)."
		}
	}
	for _, good := range likelyInstrumentedHosts {
		if host == good || strings.HasPrefix(host, good+".") || strings.Contains(raw, "://"+good) || strings.Contains(raw, "://"+good+":") {
			return true, "Target looks like a compose demo app — expect tags.load_run_id on spans when the OPA agent is ingesting that service."
		}
	}
	return false, "OPA correlation requires the target app to be instrumented (propagate X-OPA-Load-Run-Id / baggage load_run_id onto spans). Uninstrumented hosts never yield traces in Open traces."
}
