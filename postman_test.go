package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParsePostmanCollectionV21(t *testing.T) {
	// parsePostmanToSteps runs every URL through the SSRF guard, which resolves the
	// host — so without this pin the fixture needs public DNS and both steps get
	// skipped wherever there is none. See pinPerfDNS in harden_test.go.
	pinPerfDNS(t, map[string]string{"httpbin.org": "192.0.2.10"})

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

// TestParsePostmanSkipsBlockedHosts covers the other half of the import contract:
// a request the SSRF guard refuses is dropped with a warning rather than failing the
// whole parse, and the remaining requests still import.
func TestParsePostmanSkipsBlockedHosts(t *testing.T) {
	pinPerfDNS(t, map[string]string{
		"good.test":     "192.0.2.10",
		"intranet.test": "192.168.7.7",
	})

	raw := []byte(`{
  "info": { "name": "Mixed" },
  "item": [
    { "name": "Good", "request": { "method": "GET", "url": "https://good.test/ok" } },
    { "name": "Private", "request": { "method": "GET", "url": "https://intranet.test/admin" } },
    { "name": "Unresolvable", "request": { "method": "GET", "url": "https://nowhere.test/x" } },
    { "name": "Templated", "request": { "method": "GET", "url": "https://{{host}}/x" } }
  ]
}`)
	steps, _, warnings, err := parsePostmanToSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Good + Templated survive (templated URLs are deferred, not resolved).
	if len(steps) != 2 {
		t.Fatalf("steps=%d warnings=%v", len(steps), warnings)
	}
	if got := fmt.Sprint(steps[0]["url"]); got != "https://good.test/ok" {
		t.Fatalf("first step url=%q", got)
	}
	if got := fmt.Sprint(steps[1]["url"]); got != "https://${host}/x" {
		t.Fatalf("templated step url=%q", got)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "skipped Private: private/link-local address not allowed") {
		t.Fatalf("missing private-host warning: %v", warnings)
	}
	if !strings.Contains(joined, "skipped Unresolvable: dns lookup failed") {
		t.Fatalf("missing unresolvable-host warning: %v", warnings)
	}
}

func TestExpandPostmanVars(t *testing.T) {
	got := expandPostmanVars("Bearer {{token}} / {{id}}")
	if got != "Bearer ${token} / ${id}" {
		t.Fatalf("got %q", got)
	}
}
