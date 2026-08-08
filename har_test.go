package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHARImportAllowsLabPrivateNASFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "opl-har-nas-opa-dashboard.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	steps, warnings, err := parseHARToSteps(raw, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) < 1 {
		t.Fatalf("expected lab private hosts to import, got 0 steps warnings=%v", warnings)
	}
	api := 0
	for _, s := range steps {
		u, _ := s["url"].(string)
		if strings.Contains(u, "/api/") {
			api++
		}
		if !strings.Contains(u, "192.168.100.101") {
			t.Fatalf("unexpected host in %q", u)
		}
	}
	if api < 1 {
		t.Fatalf("expected ≥1 /api/ step, got %d of %d: %#v", api, len(steps), steps)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "OPA_PERF_INTERNAL_HOSTS") {
		t.Fatalf("expected INTERNAL_HOSTS honesty warning, got %v", warnings)
	}
	if !strings.Contains(joined, "skipped static=") {
		t.Fatalf("expected skip tallies in warnings, got %v", warnings)
	}
	// Dial-pin must still reject the imported private URL without allowlist.
	if err := isBlockedPerfURL(steps[0]["url"].(string)); err == nil {
		t.Fatal("isBlockedPerfURL must still block RFC1918 without OPA_PERF_INTERNAL_HOSTS")
	}
}

func TestHARImportStillBlocksCloudMetadata(t *testing.T) {
	raw := []byte(`{
  "log": {"entries": [
    {"request": {"method": "GET", "url": "http://169.254.169.254/latest/meta-data/"}},
    {"request": {"method": "GET", "url": "http://metadata.google.internal/computeMetadata/v1/"}}
  ]}
}`)
	steps, warnings, err := parseHARToSteps(raw, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("metadata must stay blocked, got %#v", steps)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "blocked=") || !strings.Contains(joined, "skipped blocked URL") {
		t.Fatalf("expected blocked tallies/warnings, got %v", warnings)
	}
}

func TestHARImportEmptyIncludesSkipTallies(t *testing.T) {
	raw := []byte(`{
  "log": {"entries": [
    {"request": {"method": "OPTIONS", "url": "http://example.com/api"}},
    {"request": {"method": "GET", "url": "http://example.com/app.js"}},
    {"request": {"method": "GET", "url": ""}}
  ]}
}`)
	steps, warnings, err := parseHARToSteps(raw, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("expected empty steps, got %#v", steps)
	}
	joined := strings.Join(warnings, " ")
	if !strings.Contains(joined, "static=") || !strings.Contains(joined, "OPTIONS=") || !strings.Contains(joined, "empty=") {
		t.Fatalf("empty import must surface tallies, got %v", warnings)
	}
}

func TestObviouslyBlockedAllowsLoopbackAndDockerInternal(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8080/api",
		"http://10.0.0.5/x",
		"http://host.docker.internal:8090/y",
		"http://localhost:3000/",
	} {
		if err := isObviouslyBlockedPerfURL(u); err != nil {
			t.Fatalf("import should allow lab URL %s: %v", u, err)
		}
		if !isLabPrivatePerfURL(u) {
			t.Fatalf("expected lab-private classification for %s", u)
		}
	}
	if err := isObviouslyBlockedPerfURL("http://169.254.169.254/"); err == nil {
		t.Fatal("metadata IP must stay hard-blocked at import")
	}
}
