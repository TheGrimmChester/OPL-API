package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFlattenScenarioStepsDepthFirst(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "transaction", "name": "LoginFlow",
			"children": []interface{}{
				map[string]interface{}{
					"type": "http", "name": "Login", "method": "POST", "url": "http://node-app:3000/login",
					"children": []interface{}{
						map[string]interface{}{"type": "extract", "name": "tok", "var": "token", "expression": "$.token", "engine": "jsonpath"},
						map[string]interface{}{"type": "assert", "name": "ok", "status": 200},
					},
				},
				map[string]interface{}{"type": "http", "name": "Home", "method": "GET", "url": "http://node-app:3000/"},
			},
		},
	}
	flat := flattenScenarioSteps(steps)
	if len(flat) != 5 {
		t.Fatalf("want 5 flat steps, got %d %#v", len(flat), flat)
	}
	if flat[0]["type"] != "transaction" || flat[1]["type"] != "http" || flat[2]["type"] != "extract" {
		t.Fatalf("order wrong: %#v", flat)
	}
	if _, ok := flat[1]["children"]; ok {
		t.Fatalf("http clone should drop children")
	}
}

func TestNestedStepsProduceTransactionAndHTTPWithExtractors(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "transaction", "name": "Checkout",
			"children": []interface{}{
				map[string]interface{}{
					"type": "http", "name": "AddToCart", "method": "POST",
					"url": "http://127.0.0.1:8080/cart",
					"children": []interface{}{
						map[string]interface{}{
							"type": "extract", "name": "CartId", "var": "cart_id",
							"engine": "jsonpath", "expression": "$.id",
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("nested", "http://127.0.0.1:8080/", "GET", "", 1, 30, raw)
	for _, want := range []string{
		`TransactionController`,
		`testname="Checkout"`,
		`HTTPSamplerProxy`,
		`testname="AddToCart"`,
		`JSONPostProcessor`,
		`JSONPostProcessor.referenceNames">cart_id`,
		`JSONPostProcessor.jsonPathExprs">$.id`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
	}
	// Extractor must sit inside the HTTP sampler hashTree (after HTTPSamplerProxy, before its closing hashTree sibling of Transaction).
	httpIdx := strings.Index(jmx, "<HTTPSamplerProxy")
	jsonIdx := strings.Index(jmx, "<JSONPostProcessor")
	if httpIdx < 0 || jsonIdx < 0 || jsonIdx < httpIdx {
		t.Fatalf("JSONPostProcessor should follow HTTPSamplerProxy in document order")
	}
	// Nested hashTree: TransactionController then <hashTree> containing HTTP, HTTP then <hashTree> containing extractor.
	txClose := strings.Index(jmx, "</TransactionController>")
	txTree := strings.Index(jmx[txClose:], "<hashTree>")
	if txClose < 0 || txTree < 0 {
		t.Fatalf("TransactionController should open a nested hashTree")
	}
}

func TestIfWhileLoopControllersJMXRoundTrip(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "if", "name": "IfOK", "condition": `${__jexl3("${status}"=="200")}`,
			"children": []interface{}{
				map[string]interface{}{
					"type": "loop", "name": "Retry", "loops": 3,
					"children": []interface{}{
						map[string]interface{}{
							"type": "while", "name": "WhileMore", "condition": `${__jexl3("${more}"=="true")}`,
							"children": []interface{}{
								map[string]interface{}{
									"type": "http", "name": "Poll", "method": "GET",
									"url": "http://127.0.0.1:8080/poll",
								},
							},
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("logic", "http://127.0.0.1:8080/", "GET", "", 2, 45, raw)
	for _, want := range []string{
		`IfController`, `testname="IfOK"`, `IfController.condition`,
		`LoopController`, `testname="Retry"`, `LoopController.loops">3`,
		`WhileController`, `testname="WhileMore"`, `WhileController.condition`,
		`HTTPSamplerProxy`, `testname="Poll"`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
	}
	sc, warnings, err := parseJMXToScenario([]byte(jmx), "logic-rt")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	_ = warnings
	imported, ok := sc["steps"].([]map[string]interface{})
	if !ok {
		// JSON round-trip may yield []interface{}
		blob, _ := json.Marshal(sc["steps"])
		if json.Unmarshal(blob, &imported) != nil || len(imported) == 0 {
			t.Fatalf("expected nested steps, got %#v", sc["steps"])
		}
	}
	if len(imported) != 1 || fmt.Sprint(imported[0]["type"]) != "if" {
		t.Fatalf("want root if, got %#v", imported)
	}
	ifKids := stepChildren(imported[0])
	if len(ifKids) != 1 || fmt.Sprint(ifKids[0]["type"]) != "loop" {
		t.Fatalf("want loop under if, got %#v", ifKids)
	}
	if int(asFloatOr(ifKids[0]["loops"], 0)) != 3 {
		t.Fatalf("want loops=3, got %#v", ifKids[0]["loops"])
	}
	loopKids := stepChildren(ifKids[0])
	if len(loopKids) != 1 || fmt.Sprint(loopKids[0]["type"]) != "while" {
		t.Fatalf("want while under loop, got %#v", loopKids)
	}
	whileKids := stepChildren(loopKids[0])
	if len(whileKids) != 1 || fmt.Sprint(whileKids[0]["type"]) != "http" {
		t.Fatalf("want http under while, got %#v", whileKids)
	}
	if !strings.Contains(fmt.Sprint(whileKids[0]["url"]), "/poll") {
		t.Fatalf("poll url lost: %#v", whileKids[0]["url"])
	}
}

func TestForeachFragmentIncludeJMX(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "fragment", "name": "LoginFrag",
			"children": []interface{}{
				map[string]interface{}{
					"type": "http", "name": "Login", "method": "POST",
					"url": "http://127.0.0.1:8080/login",
				},
			},
		},
		{
			"type": "include", "name": "UseLogin", "ref": "LoginFrag",
		},
		{
			"type": "foreach", "name": "EachItem", "input_var": "ids", "return_var": "id",
			"children": []interface{}{
				map[string]interface{}{
					"type": "http", "name": "GetItem", "method": "GET",
					"url": "http://127.0.0.1:8080/items/${id}",
				},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("frag", "http://127.0.0.1:8080/", "GET", "", 1, 30, raw)
	for _, want := range []string{
		`<TestFragmentController`, `testname="LoginFrag"`, `opl.fragment`,
		`enabled="false"`,
		`<ModuleController`, `ModuleController.node_path`,
		`testname="Login"`,
		`ForeachController`, `ForeachController.inputVal">ids`, `ForeachController.returnVal">id`,
		`testname="GetItem"`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
	}
	// The reusable journey is stored once by reference, not copied into the flow.
	if got := strings.Count(jmx, `testname="Login"`); got != 1 {
		t.Fatalf("want the Login sampler emitted once (inside the fragment), got %d\n%s", got, jmx)
	}
	flat := flattenScenarioSteps(steps)
	// fragment skipped; include expands Login http; foreach marker + GetItem
	if len(flat) < 3 {
		t.Fatalf("flat=%#v", flat)
	}
	types := []string{}
	for _, s := range flat {
		types = append(types, fmt.Sprint(s["type"]))
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "http") || !strings.Contains(joined, "foreach") {
		t.Fatalf("unexpected flat types %s", joined)
	}

	// Fragments are Test Plan level containers, so the full import merges them with the
	// thread group tree.
	sc, _, err := parseJMXToScenario([]byte(jmx), "frag-rt")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	imported := importedSteps(t, sc)
	if len(imported) == 0 {
		t.Fatal("expected import tree")
	}
	foundFrag, foundForeach, foundRef := false, false, false
	for _, s := range imported {
		switch fmt.Sprint(s["type"]) {
		case "fragment":
			foundFrag = true
		case "include":
			foundRef = true
			if fmt.Sprint(s["ref"]) != "LoginFrag" {
				t.Fatalf("module reference lost its target: %#v", s)
			}
		case "foreach":
			foundForeach = true
			if fmt.Sprint(s["input_var"]) != "ids" {
				t.Fatalf("input_var=%v", s["input_var"])
			}
		}
	}
	if !foundFrag || !foundForeach || !foundRef {
		t.Fatalf("import missing fragment/reference/foreach: %#v", imported)
	}
}

func TestSuggestAutoCorrelation(t *testing.T) {
	results := []map[string]interface{}{
		{
			"type": "http", "name": "Login", "ok": true,
			"body_preview": `{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc","user":"a"}`,
		},
	}
	sugs := suggestAutoCorrelation(results)
	if len(sugs) == 0 {
		t.Fatal("expected suggestions")
	}
	if fmt.Sprint(sugs[0]["engine"]) != "jsonpath" || !strings.Contains(fmt.Sprint(sugs[0]["expression"]), "access_token") {
		t.Fatalf("%#v", sugs[0])
	}
}

func TestApplyLoadCurveToSchedule(t *testing.T) {
	sched := map[string]interface{}{}
	peak, dur, honesty := applyLoadCurveToSchedule([]loadCurvePoint{
		{TSec: 0, VUs: 0},
		{TSec: 30, VUs: 20},
		{TSec: 90, VUs: 20},
		{TSec: 120, VUs: 0},
	}, sched)
	if peak != 20 || dur != 120 {
		t.Fatalf("peak=%d dur=%d sched=%#v", peak, dur, sched)
	}
	if int(asFloatOr(sched["ramp_seconds"], -1)) != 30 {
		t.Fatalf("ramp_seconds=%v", sched["ramp_seconds"])
	}
	if honesty == "" {
		t.Fatal("expected honesty string")
	}
	if fmt.Sprint(sched["policy"]) != "custom" {
		t.Fatalf("policy=%v", sched["policy"])
	}
	if fmt.Sprint(sched["curve_mode"]) != "vus" {
		t.Fatalf("curve_mode=%v", sched["curve_mode"])
	}
}

func TestApplyArrivalsCurveToSchedule(t *testing.T) {
	sched := map[string]interface{}{"curve_mode": "arrivals"}
	total, dur, honesty := applyLoadCurveToSchedule([]loadCurvePoint{
		{TSec: 0, Rate: 0},
		{TSec: 10, Rate: 2},
		{TSec: 20, Rate: 2},
		{TSec: 30, Rate: 0},
	}, sched)
	// Trapezoids: (0+2)/2*10=10, (2+2)/2*10=20, (2+0)/2*10=10 → 40
	if total != 40 {
		t.Fatalf("total=%d want 40 sched=%#v", total, sched)
	}
	if dur != 30 {
		t.Fatalf("dur=%d", dur)
	}
	if honesty == "" || !strings.Contains(honesty, "Arrivals") {
		t.Fatalf("honesty=%q", honesty)
	}
	segs := arrivalSegmentsFromSched(sched)
	if len(segs) != 3 {
		t.Fatalf("segs=%#v", segs)
	}
	sum := 0
	for _, s := range segs {
		sum += s.Arrivals
	}
	if sum != 40 {
		t.Fatalf("sum=%d", sum)
	}
	jmx := generateJMXFromUpsertEx("arr", "http://127.0.0.1/", "GET", "", 0, dur, json.RawMessage(`[{"type":"http","name":"R","method":"GET","url":"http://127.0.0.1/"}]`), sched)
	if !strings.Contains(jmx, `LoopController.loops">1`) {
		t.Fatalf("expected open-model loops=1: %s", jmx[:min(400, len(jmx))])
	}
	if !strings.Contains(jmx, "ThreadGroup.delay") {
		t.Fatal("expected ThreadGroup.delay")
	}
	if strings.Count(jmx, "<ThreadGroup ") < 3 {
		t.Fatalf("expected ≥3 ThreadGroups, got %d", strings.Count(jmx, "<ThreadGroup "))
	}
}

func TestScaleArrivalSegments(t *testing.T) {
	segs := []arrivalSegment{
		{DelaySec: 0, RampSec: 10, Arrivals: 10},
		{DelaySec: 10, RampSec: 10, Arrivals: 30},
	}
	out := scaleArrivalSegments(segs, 20, 40)
	sum := 0
	for _, s := range out {
		sum += s.Arrivals
	}
	if sum != 20 {
		t.Fatalf("sum=%d out=%#v", sum, out)
	}
}

func TestHTTPHeadersEmitHeaderManager(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "http", "name": "Auth", "method": "GET",
			"url": "http://127.0.0.1:8080/api",
			"headers": []interface{}{
				map[string]interface{}{"name": "Authorization", "value": "Bearer ${token}"},
				map[string]interface{}{"name": "Accept", "value": "application/json"},
			},
			"children": []interface{}{
				map[string]interface{}{"type": "assert", "name": "ok", "status": 200},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("hdrs", "http://127.0.0.1:8080/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `testname="OPA Correlation Headers"`) {
		t.Fatal("plan-level OPA correlation HeaderManager must remain")
	}
	httpIdx := strings.Index(jmx, `<HTTPSamplerProxy`)
	stepHM := strings.Index(jmx, `testname="HTTP Headers"`)
	assertIdx := strings.Index(jmx, `<ResponseAssertion`)
	if httpIdx < 0 || stepHM < 0 || assertIdx < 0 {
		t.Fatalf("missing sampler/headers/assert markers\n%s", jmx)
	}
	if !(httpIdx < stepHM && stepHM < assertIdx) {
		t.Fatalf("per-step HeaderManager must sit under sampler hashTree before assert (http=%d hm=%d assert=%d)", httpIdx, stepHM, assertIdx)
	}
	if !strings.Contains(jmx, `Header.name">Authorization`) || !strings.Contains(jmx, `Header.value">Bearer ${token}`) {
		t.Fatalf("missing Authorization header emit\n%s", jmx)
	}
}

func TestRampSecondsEmittedOnThreadGroup(t *testing.T) {
	steps := json.RawMessage(`[{"type":"http","name":"R","method":"GET","url":"http://127.0.0.1/"}]`)
	jmxDefault := generateJMXFromUpsertData("r", "http://127.0.0.1/", "GET", "", 5, 60, steps, nil, nil)
	if !strings.Contains(jmxDefault, `ThreadGroup.ramp_time">10</stringProp>`) {
		t.Fatalf("default ramp should stay 10:\n%s", jmxDefault)
	}
	sched := map[string]interface{}{"ramp_seconds": 45}
	jmx := generateJMXFromUpsertData("r", "http://127.0.0.1/", "GET", "", 5, 60, steps, sched, nil)
	if !strings.Contains(jmx, `ThreadGroup.ramp_time">45</stringProp>`) {
		t.Fatalf("expected ramp_seconds=45 on ThreadGroup:\n%s", jmx)
	}
}

func TestDisabledHTTPEmitsEnabledFalse(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "http", "name": "Off", "method": "GET", "url": "http://127.0.0.1/off", "enabled": false},
		{"type": "http", "name": "On", "method": "GET", "url": "http://127.0.0.1/on"},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("en", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `testname="Off" enabled="false"`) {
		t.Fatalf("disabled step should emit enabled=false:\n%s", jmx)
	}
	if !strings.Contains(jmx, `testname="On" enabled="true"`) {
		t.Fatalf("enabled step should emit enabled=true:\n%s", jmx)
	}
	flat := flattenScenarioSteps(steps)
	if len(flat) != 1 || flat[0]["name"] != "On" {
		t.Fatalf("validate flatten should skip disabled: %#v", flat)
	}
}

func TestFollowRedirectsFalse(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "http", "name": "NoRedir", "method": "GET", "url": "http://127.0.0.1/x", "follow_redirects": false},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("fr", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `HTTPSampler.follow_redirects">false</boolProp>`) {
		t.Fatalf("expected follow_redirects false:\n%s", jmx)
	}
}

func TestValidateTriageIncludesPath(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "transaction", "name": "Flow",
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": "Fail", "method": "GET", "url": "http://127.0.0.1:1/nope"},
			},
		},
	}
	flat := flattenScenarioSteps(steps)
	if len(flat) < 2 {
		t.Fatalf("flat=%#v", flat)
	}
	httpStep := flat[1]
	path, ok := httpStep["path"].([]interface{})
	if !ok || len(path) != 3 || path[0] != 0 || path[1] != "children" || path[2] != 0 {
		t.Fatalf("want nestable path [0 children 0], got %#v", httpStep["path"])
	}
	results := []map[string]interface{}{
		{"type": "http", "name": "Fail", "ok": false, "error": "connection refused", "path": path},
	}
	_, triage := triageValidateResults(results)
	if len(triage) != 1 {
		t.Fatalf("triage=%#v", triage)
	}
	tp, ok := triage[0]["path"].([]interface{})
	if !ok || len(tp) != 3 || tp[2] != 0 {
		t.Fatalf("triage path=%#v", triage[0]["path"])
	}
}

func TestThinkMsRandEmitsUniformRandomTimer(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "http", "name": "T", "method": "GET", "url": "http://127.0.0.1/", "think_ms": 100, "think_ms_rand": 400},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("tr", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, "UniformRandomTimer") {
		t.Fatalf("expected UniformRandomTimer:\n%s", jmx)
	}
	if !strings.Contains(jmx, `ConstantTimer.delay">100</stringProp>`) || !strings.Contains(jmx, `RandomTimer.range">300</stringProp>`) {
		t.Fatalf("expected delay=100 range=300:\n%s", jmx)
	}
}

func TestEmptyHeadersSkipStepHeaderManager(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "http", "name": "Bare", "method": "GET", "url": "http://127.0.0.1/", "headers": map[string]interface{}{}},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("eh", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if strings.Contains(jmx, `testname="HTTP Headers"`) {
		t.Fatalf("empty headers must not emit step HeaderManager:\n%s", jmx)
	}
	if !strings.Contains(jmx, `testname="OPA Correlation Headers"`) {
		t.Fatal("correlation HeaderManager must remain")
	}
}

func TestHTTPAdvancedTimeoutsAndEncode(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "http", "name": "Adv", "method": "POST", "url": "http://127.0.0.1/x",
			"body": `{"a":1}`, "always_encode": true, "connect_timeout_ms": 1500, "response_timeout_ms": 9000,
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("adv", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `HTTPArgument.always_encode">true</boolProp>`) {
		t.Fatalf("always_encode true missing:\n%s", jmx)
	}
	if !strings.Contains(jmx, `HTTPSampler.connect_timeout">1500</stringProp>`) {
		t.Fatalf("connect_timeout missing:\n%s", jmx)
	}
	if !strings.Contains(jmx, `HTTPSampler.response_timeout">9000</stringProp>`) {
		t.Fatalf("response_timeout missing:\n%s", jmx)
	}
}

func TestExtractAdvancedPropsEmit(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "http", "name": "H", "method": "GET", "url": "http://127.0.0.1/",
			"children": []interface{}{
				map[string]interface{}{
					"type": "extract", "name": "Tok", "engine": "regex",
					"expression": `"token"\s*:\s*"([^"]+)"`, "var": "token",
					"match_number": 2, "template": "$1$", "default_value": "none",
				},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("ex", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `RegexExtractor.match_number">2</stringProp>`) {
		t.Fatalf("match_number:\n%s", jmx)
	}
	if !strings.Contains(jmx, `RegexExtractor.template">$1$</stringProp>`) {
		t.Fatalf("template:\n%s", jmx)
	}
	if !strings.Contains(jmx, `RegexExtractor.default">none</stringProp>`) {
		t.Fatalf("default:\n%s", jmx)
	}
}

func TestAssertAdvancedPropsEmit(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "assert", "name": "BodyOK", "body_contains": "ready",
			"assert_type": "equals", "assert_field": "response_data", "assume_success": true,
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("as", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `Assertion.assume_success">true</boolProp>`) {
		t.Fatalf("assume_success:\n%s", jmx)
	}
	if !strings.Contains(jmx, `Assertion.test_field">Assertion.response_data</stringProp>`) {
		t.Fatalf("assert_field:\n%s", jmx)
	}
}

func TestTransactionAdvancedPropsEmit(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "transaction", "name": "Txn",
			"include_timers": true, "generate_parent_sample": true,
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": "H", "method": "GET", "url": "http://127.0.0.1/"},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("txn", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `TransactionController.includeTimers">true</boolProp>`) {
		t.Fatalf("includeTimers:\n%s", jmx)
	}
	if !strings.Contains(jmx, `TransactionController.parent">true</boolProp>`) {
		t.Fatalf("generate parent sample:\n%s", jmx)
	}
}

func TestIfControllerAdvancedPropsEmit(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "if", "name": "Gate", "condition": `${__jexl3(true)}`,
			"use_expression": false, "evaluate_all": true,
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": "H", "method": "GET", "url": "http://127.0.0.1/"},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("if", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `IfController.evaluateAll">true</boolProp>`) {
		t.Fatalf("evaluateAll:\n%s", jmx)
	}
	if !strings.Contains(jmx, `IfController.useExpression">false</boolProp>`) {
		t.Fatalf("useExpression false:\n%s", jmx)
	}
}

func TestForEachUseSeparatorEmit(t *testing.T) {
	steps := []map[string]interface{}{
		{
			"type": "foreach", "name": "Each", "input_var": "items", "return_var": "item",
			"use_separator": false,
			"children": []interface{}{
				map[string]interface{}{"type": "http", "name": "H", "method": "GET", "url": "http://127.0.0.1/${item}"},
			},
		},
	}
	raw, _ := json.Marshal(steps)
	jmx := generateJMXFromUpsert("fe", "http://127.0.0.1/", "GET", "", 1, 30, raw)
	if !strings.Contains(jmx, `ForeachController.useSeparator">false</boolProp>`) {
		t.Fatalf("useSeparator false:\n%s", jmx)
	}
}
