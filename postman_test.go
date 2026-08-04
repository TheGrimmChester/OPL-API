package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParsePostmanCollectionV21(t *testing.T) {
	raw := []byte(`{
  "info": { "name": "Demo API", "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" },
  "item": [
    {
      "name": "Auth",
      "item": [
        {
          "name": "Login",
          "request": {
            "method": "POST",
            "header": [{ "key": "Content-Type", "value": "application/json" }],
            "body": { "mode": "raw", "raw": "{\"user\":\"{{user}}\"}" },
            "url": "https://httpbin.org/post"
          }
        }
      ]
    },
    {
      "name": "Hello",
      "request": {
        "method": "GET",
        "url": { "raw": "https://httpbin.org/get?x=1", "protocol": "https", "host": ["httpbin","org"], "path": ["get"], "query": [{"key":"x","value":"1"}] }
      }
    }
  ]
}`)
	steps, name, warnings, err := parsePostmanToSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Demo API" {
		t.Fatalf("name=%q", name)
	}
	if len(steps) != 2 {
		t.Fatalf("steps=%d warnings=%v", len(steps), warnings)
	}
	if fmt.Sprint(steps[0]["method"]) != "POST" || !strings.Contains(fmt.Sprint(steps[0]["url"]), "httpbin.org/post") {
		t.Fatalf("login step %#v", steps[0])
	}
	body := fmt.Sprint(steps[0]["body"])
	if !strings.Contains(body, "${user}") {
		t.Fatalf("expected {{user}}→${user}, got %q", body)
	}
	if fmt.Sprint(steps[1]["method"]) != "GET" {
		t.Fatalf("hello %#v", steps[1])
	}
}

func TestExpandPostmanVars(t *testing.T) {
	got := expandPostmanVars("Bearer {{token}} / {{id}}")
	if got != "Bearer ${token} / ${id}" {
		t.Fatalf("got %q", got)
	}
}
