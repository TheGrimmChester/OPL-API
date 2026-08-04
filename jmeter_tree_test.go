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
		`GenericController`, `Fragment:LoginFrag`, `opl.fragment`,
		`enabled="false"`,
		`<!-- opl-include ref=LoginFrag -->`,
		`testname="Login"`,
		`ForeachController`, `ForeachController.inputVal">ids`, `ForeachController.returnVal">id`,
		`testname="GetItem"`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
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

	imported := extractStepsFromJMXTree([]byte(jmx))
	if len(imported) == 0 {
		t.Fatal("expected import tree")
	}
	foundFrag, foundForeach := false, false
	for _, s := range imported {
		if fmt.Sprint(s["type"]) == "fragment" {
			foundFrag = true
		}
		if fmt.Sprint(s["type"]) == "foreach" {
			foundForeach = true
			if fmt.Sprint(s["input_var"]) != "ids" {
				t.Fatalf("input_var=%v", s["input_var"])
			}
		}
	}
	if !foundFrag || !foundForeach {
		t.Fatalf("import missing fragment/foreach: %#v", imported)
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
}
