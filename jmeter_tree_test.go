package main

import (
	"encoding/json"
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
