package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunNotifyMode(t *testing.T) {
	t.Setenv("OPL_RUN_NOTIFY_MODE", "")
	if got := runNotifyMode(); got != "deliver" {
		t.Fatalf("default = %q", got)
	}
	for _, v := range []string{"log", "LOG-ONLY", "dry-run", "dryrun"} {
		t.Setenv("OPL_RUN_NOTIFY_MODE", v)
		if got := runNotifyMode(); got != "log" {
			t.Fatalf("%q => %q", v, got)
		}
	}
}

func TestRunNotifyStatusAllowed(t *testing.T) {
	t.Setenv("OPL_RUN_NOTIFY_STATUSES", "")
	if !runNotifyStatusAllowed("failed") || !runNotifyStatusAllowed("completed") {
		t.Fatal("default should allow all terminal")
	}
	if runNotifyStatusAllowed("running") {
		t.Fatal("running must not notify")
	}
	t.Setenv("OPL_RUN_NOTIFY_STATUSES", "failed,cancelled")
	if !runNotifyStatusAllowed("failed") || !runNotifyStatusAllowed("cancelled") {
		t.Fatal("filter should allow listed")
	}
	if runNotifyStatusAllowed("passed") {
		t.Fatal("passed not in filter")
	}
}

func TestRedactWebhookHost(t *testing.T) {
	got := redactWebhookHost("https://hooks.example.com/secret/path?token=abc")
	if got != "https://hooks.example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestDeliverRunNotifyLogMode(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_NOTIFY_MODE", "log")
	st := deliverRunNotify(runNotifyEvent{RunID: "r1", Status: "failed", Source: "test"})
	if st != "logged" {
		t.Fatalf("status=%q", st)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("log mode must not POST")
	}
}

func TestDeliverRunNotifySuccessAndSignature(t *testing.T) {
	secret := "test-secret"
	var (
		mu      sync.Mutex
		gotBody []byte
		gotSig  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = raw
		gotSig = r.Header.Get("X-OPL-Signature")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_NOTIFY_MODE", "deliver")
	t.Setenv("OPL_RUN_WEBHOOK_SECRET", secret)
	t.Setenv("OPL_PUBLIC_URL", "https://lab.example.com")

	st := deliverRunNotify(runNotifyEvent{
		RunID: "run-abc", ScenarioID: "scn-1", OrganizationID: "default-org",
		ProjectID: "default-project", Status: "failed", VUs: 10, Error: "sla",
		Source: "metrics", Summary: map[string]interface{}{"error_rate": 0.2},
	})
	if st != "sent" {
		t.Fatalf("status=%q want sent", st)
	}
	mu.Lock()
	defer mu.Unlock()
	var payload map[string]interface{}
	if json.Unmarshal(gotBody, &payload) != nil {
		t.Fatalf("bad json: %s", gotBody)
	}
	if payload["event"] != "opl.run.terminal" || payload["run_id"] != "run-abc" || payload["status"] != "failed" {
		t.Fatalf("payload=%#v", payload)
	}
	if payload["run_url"] == nil || payload["run_url"] == "" {
		t.Fatal("expected run_url from OPL_PUBLIC_URL")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("sig=%q want %q", gotSig, want)
	}
}

func TestDeliverRunNotifyHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_NOTIFY_MODE", "deliver")
	t.Setenv("OPL_RUN_WEBHOOK_SECRET", "")
	if st := deliverRunNotify(runNotifyEvent{RunID: "r2", Status: "failed"}); st != "failed" {
		t.Fatalf("status=%q", st)
	}
}

func TestNotifyRunTerminalDedupAndAsync(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_NOTIFY_MODE", "deliver")
	t.Setenv("OPL_RUN_NOTIFY_STATUSES", "")
	runNotifyOnceDedup = sync.Map{}

	evt := runNotifyEvent{RunID: "dedup-1", Status: "cancelled", Source: "cancel"}
	notifyRunTerminal(evt)
	notifyRunTerminal(evt) // same run+status — deduped
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("hits=%d want 1", n)
	}
}

func TestRunNotifyStatusInfo(t *testing.T) {
	t.Setenv("OPL_RUN_WEBHOOK_URL", "")
	info := runNotifyStatusInfo()
	if info["configured"] != false {
		t.Fatalf("%#v", info)
	}
	t.Setenv("OPL_RUN_WEBHOOK_URL", "https://hooks.example.com/x")
	t.Setenv("OPL_RUN_NOTIFY_MODE", "log")
	info = runNotifyStatusInfo()
	if info["configured"] != true || info["mode"] != "log" {
		t.Fatalf("%#v", info)
	}
}
