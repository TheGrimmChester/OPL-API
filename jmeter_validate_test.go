package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// validateScenarioRow seeds a scenario the validate handler can load, with a step
// tree supplied as steps_json.
func validateScenarioRow(id, targetURL, stepsJSON string) map[string]interface{} {
	ts := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05.000")
	return map[string]interface{}{
		"id": id, "organization_id": "org", "project_id": "proj",
		"name": "validate-" + id, "target_url": targetURL, "method": "GET",
		"vus": 1, "duration_seconds": 30,
		"headers_json": "{}", "body": "", "thresholds_json": "{}",
		"steps_json": stepsJSON, "datasets_json": "{}", "sla_json": "{}",
		"schedule_json": "{}", "jmx_xml": "", "archived": 0,
		"updated_at": ts, "created_at": ts,
	}
}

// countingTarget is a target that records how many requests validate actually sent,
// with the paths it hit, so a spurious sample cannot hide in a passing response.
func countingTarget(t *testing.T) (*httptest.Server, *int64, *[]string) {
	t.Helper()
	var hits int64
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	// The dry run pins its dial to non-private IPs; a loopback test target has to be
	// declared internal the same way a compose service would be.
	t.Setenv("OPA_PERF_INTERNAL_HOSTS", "127.0.0.1")
	return srv, &hits, &paths
}

func runValidate(t *testing.T, id string) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	handlePerfScenarioValidate(rec, httptest.NewRequest(http.MethodPost, "/api/perf/scenarios/"+id+"/validate", nil), id)
	if rec.Code != 200 {
		t.Fatalf("validate returned %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("validate response is not JSON: %s", rec.Body.String())
	}
	return out
}

func validateSteps(t *testing.T, body map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, _ := body["steps"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("step is not an object: %#v", r)
		}
		out = append(out, m)
	}
	return out
}

// TestValidateDoesNotSampleLogicControllers is the regression test for the bug this
// change fixes.
//
// flattenScenarioSteps keeps if/while/loop/foreach in the flat list as structural
// markers so validate can report journey shape. The handler's switch had no case for
// them, so each one fell through to the HTTP branch, where step["url"] is nil and the
// fallback is sc["target_url"] — every controller fired a real extra request at the
// target on every validate, and came back in steps[] as an HTTP sample with a status
// code and a latency that fed triage and correlation.
func TestValidateDoesNotSampleLogicControllers(t *testing.T) {
	f := wireFakeClickHouse(t)
	srv, hits, paths := countingTarget(t)

	// One controller of every kind, each wrapping the single real request. Conditions
	// stay on plan built-ins so this exercises the controller path and not the
	// separate unbound-${…} check.
	steps := []map[string]interface{}{
		{
			"type": "if", "name": "IfOK", "condition": `${__jexl3(1==1)}`,
			"children": []interface{}{
				map[string]interface{}{
					"type": "while", "name": "WhileMore", "condition": `${__jexl3(false)}`,
					"children": []interface{}{
						map[string]interface{}{
							"type": "loop", "name": "Retry", "loops": 3,
							"children": []interface{}{
								map[string]interface{}{
									"type": "foreach", "name": "EachID", "input_var": "ids", "return_var": "id",
									"children": []interface{}{
										map[string]interface{}{
											"type": "http", "name": "GetItem", "method": "GET",
											"url": srv.URL + "/items",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	f.seed("load_scenarios", validateScenarioRow("scn-ctl", srv.URL+"/", string(raw)))

	body := runValidate(t, "scn-ctl")

	// The journey owns exactly one request. Four controllers must add none.
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("validate sent %d requests, want 1 (the single http step); paths=%v", got, *paths)
	}
	for _, p := range *paths {
		if p != "/items" {
			t.Fatalf("validate hit %q — a controller fell back to target_url", p)
		}
	}

	results := validateSteps(t, body)
	if len(results) != 5 {
		t.Fatalf("want 5 steps (4 controllers + 1 http), got %d: %#v", len(results), results)
	}
	wantTypes := []string{"if", "while", "loop", "foreach", "http"}
	for i, want := range wantTypes {
		if got := fmt.Sprint(results[i]["type"]); got != want {
			t.Fatalf("step %d type = %q, want %q: %#v", i, got, want, results[i])
		}
	}
	// A controller is structure: no sampler fields may appear on it.
	for _, step := range results[:4] {
		for _, banned := range []string{"url", "method", "status_code", "latency_ms", "body_preview"} {
			if v, present := step[banned]; present {
				t.Fatalf("controller %v carries sampler field %s=%v", step["name"], banned, v)
			}
		}
		if ok, _ := step["ok"].(bool); !ok {
			t.Fatalf("controller %v should report ok=true, got %#v", step["name"], step)
		}
	}

	// The shape the controller was designed with is reported, not dropped.
	if got := fmt.Sprint(results[0]["condition"]); got != `${__jexl3(1==1)}` {
		t.Fatalf("if condition = %q", got)
	}
	if got := fmt.Sprint(results[1]["condition"]); got != `${__jexl3(false)}` {
		t.Fatalf("while condition = %q", got)
	}
	if got := getFloat64(results[2], "loops"); int(got) != 3 {
		t.Fatalf("loop loops = %v", results[2]["loops"])
	}
	if got := fmt.Sprint(results[3]["input_var"]); got != "ids" {
		t.Fatalf("foreach input_var = %q", got)
	}
	if got := fmt.Sprint(results[3]["return_var"]); got != "id" {
		t.Fatalf("foreach return_var = %q", got)
	}

	// Controllers are not samples, so they must not manufacture triage entries or
	// correlation suggestions.
	if triage, _ := body["triage"].([]interface{}); len(triage) != 0 {
		t.Fatalf("clean journey produced triage: %#v", triage)
	}
	sugs, _ := body["correlation_suggestions"].([]interface{})
	for _, s := range sugs {
		m, _ := s.(map[string]interface{})
		if idx := int(getFloat64(m, "step_index")); idx != 4 {
			t.Fatalf("correlation suggestion attributed to non-http step %d: %#v", idx, m)
		}
	}
	if body["pass"] != true {
		t.Fatalf("expected pass, got %v (triage %#v)", body["pass"], body["triage"])
	}
}

// A controller alias spelling must take the same path as its canonical form —
// the flatten list and the validate switch are driven off one predicate.
func TestValidateControllerAliasesAreNotSampled(t *testing.T) {
	f := wireFakeClickHouse(t)
	srv, hits, paths := countingTarget(t)

	steps := []map[string]interface{}{
		{"type": "if_controller", "name": "If2", "condition": "${__jexl3(true)}"},
		{"type": "while_controller", "name": "While2", "condition": "${__jexl3(false)}"},
		{"type": "loop_controller", "name": "Loop2", "loops": 2, "forever": true},
		{"type": "foreach_controller", "name": "Each2", "input_var": "xs"},
		{"type": "for_each", "name": "Each3", "input_var": "ys"},
		{"type": "http", "name": "Only", "method": "GET", "url": srv.URL + "/only"},
	}
	raw, _ := json.Marshal(steps)
	f.seed("load_scenarios", validateScenarioRow("scn-alias", srv.URL+"/", string(raw)))

	body := runValidate(t, "scn-alias")

	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("validate sent %d requests, want 1; paths=%v", got, *paths)
	}
	results := validateSteps(t, body)
	if len(results) != 6 {
		t.Fatalf("want 6 steps, got %d: %#v", len(results), results)
	}
	for i, want := range []string{"if_controller", "while_controller", "loop_controller",
		"foreach_controller", "for_each", "http"} {
		if got := fmt.Sprint(results[i]["type"]); got != want {
			t.Fatalf("step %d type = %q, want %q", i, got, want)
		}
	}
	if results[2]["forever"] != true {
		t.Fatalf("loop forever not reported: %#v", results[2])
	}
}

// A controller that carries a method — hand-authored JSON, or a step copied from an
// HTTP one in the editor — is the shape that made the old code send a real extra
// request: the HTTP branch found a usable method and fell back to target_url for the
// URL. Without a method it merely failed the journey with an invalid-method error.
// Both are wrong, and the request-firing one is the reason this is not cosmetic.
func TestValidateControllerWithMethodFiresNoRequest(t *testing.T) {
	f := wireFakeClickHouse(t)
	srv, hits, paths := countingTarget(t)

	steps := []map[string]interface{}{
		{
			"type": "loop", "name": "Retry", "loops": 2, "method": "POST",
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": "Ping", "method": "GET", "url": srv.URL + "/ping"},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	f.seed("load_scenarios", validateScenarioRow("scn-method", srv.URL+"/", string(raw)))

	body := runValidate(t, "scn-method")

	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("validate sent %d requests, want 1 — the loop controller was sampled; paths=%v", got, *paths)
	}
	for _, p := range *paths {
		if p == "/" {
			t.Fatal("a controller fell back to target_url and hit the target")
		}
	}
	results := validateSteps(t, body)
	if len(results) != 2 || fmt.Sprint(results[0]["type"]) != "loop" {
		t.Fatalf("want [loop, http], got %#v", results)
	}
	// The stray method must not be echoed as if the controller were a sampler.
	if _, present := results[0]["method"]; present {
		t.Fatalf("controller marker echoed a method: %#v", results[0])
	}
	if body["pass"] != true {
		t.Fatalf("expected pass, got %v (triage %#v)", body["pass"], body["triage"])
	}
}

// Every controller flattenScenarioSteps keeps as a marker must be one the validate
// switch recognises, so the two lists cannot drift apart and reintroduce the bug.
func TestFlattenedControllerMarkersAreRecognisedByValidate(t *testing.T) {
	all := []string{"if", "if_controller", "while", "while_controller", "loop",
		"loop_controller", "foreach", "foreach_controller", "for_each"}
	steps := make([]map[string]interface{}, 0, len(all))
	for _, typ := range all {
		steps = append(steps, map[string]interface{}{
			"type": typ, "name": typ,
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": typ + "-body", "url": "http://node-app:3000/x"},
			},
		})
	}
	flat := flattenScenarioSteps(steps)
	markers := 0
	for _, s := range flat {
		typ := fmt.Sprint(s["type"])
		if typ == "http" {
			continue
		}
		if !isPerfControllerMarkerType(typ) {
			t.Fatalf("flatten emits %q, which validate would sample as HTTP", typ)
		}
		markers++
		if _, present := s["children"]; present {
			t.Fatalf("marker %q kept children it no longer owns", typ)
		}
	}
	if markers != len(all) {
		t.Fatalf("want %d controller markers, got %d: %#v", len(all), markers, flat)
	}
}

// TestValidateSendsStepHeadersToTarget proves dry-run validate applies step headers
// (map or array) onto the outbound request, not only scenario-level headers_json.
func TestValidateSendsStepHeadersToTarget(t *testing.T) {
	f := wireFakeClickHouse(t)
	var gotAuth, gotAccept string
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPA_PERF_INTERNAL_HOSTS", "127.0.0.1")

	steps := []map[string]interface{}{
		{
			"type": "http", "name": "Authed", "method": "GET",
			"url": srv.URL + "/secure",
			"headers": map[string]interface{}{
				"Authorization": "Bearer secret-token",
				"Accept":        "application/json",
			},
		},
	}
	raw, _ := json.Marshal(steps)
	f.seed("load_scenarios", validateScenarioRow("scn-hdrs", srv.URL+"/", string(raw)))

	body := runValidate(t, "scn-hdrs")
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("want 1 validate hit, got %d body=%v", hits, body)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q", gotAccept)
	}
}
