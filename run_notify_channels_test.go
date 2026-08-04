package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// clearNotifyEnv puts every channel back to "not configured" for a clean start.
func clearNotifyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OPL_RUN_WEBHOOK_URL", "OPL_RUN_WEBHOOK_SECRET", "OPL_RUN_CHAT_WEBHOOK_URL",
		"OPL_RUN_EMAIL_TO", "OPL_RUN_EMAIL_FROM", "OPL_RUN_NOTIFY_CHANNELS",
		"OPL_RUN_NOTIFY_MODE", "OPL_RUN_NOTIFY_STATUSES", "OPL_PUBLIC_URL",
		"OPA_SMTP_HOST", "OPA_SMTP_PORT", "OPA_SMTP_USER", "OPA_SMTP_PASS", "OPA_SMTP_FROM",
	} {
		t.Setenv(k, "")
	}
}

func resultsByChannel(results []runNotifyResult) map[string]runNotifyResult {
	out := map[string]runNotifyResult{}
	for _, r := range results {
		out[r.Channel] = r
	}
	return out
}

func TestUnconfiguredChannelsAreSkippedWithPlainReason(t *testing.T) {
	clearNotifyEnv(t)
	results := deliverRunNotifyAll(runNotifyEvent{RunID: "r-none", Status: "failed"})
	if len(results) != len(runNotifyChannels) {
		t.Fatalf("expected one result per channel, got %d", len(results))
	}
	by := resultsByChannel(results)
	for _, ch := range runNotifyChannels {
		res, ok := by[ch]
		if !ok {
			t.Fatalf("channel %q missing from results", ch)
		}
		if res.Status != "skipped" {
			t.Fatalf("%s status=%q want skipped", ch, res.Status)
		}
		if res.Detail == "" || !strings.Contains(res.Detail, "not configured") {
			t.Fatalf("%s must state plainly why nothing was sent, got %q", ch, res.Detail)
		}
	}
}

func TestChannelFilterDisablesChannel(t *testing.T) {
	clearNotifyEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_CHAT_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_NOTIFY_CHANNELS", "chat")

	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{RunID: "r-filter", Status: "failed"}))
	if by["chat"].Status != "sent" {
		t.Fatalf("chat=%#v", by["chat"])
	}
	if by["webhook"].Status != "skipped" || !strings.Contains(by["webhook"].Detail, "disabled") {
		t.Fatalf("webhook should be reported as disabled: %#v", by["webhook"])
	}
	if !runNotifyChannelEnabled("chat") || runNotifyChannelEnabled("webhook") {
		t.Fatal("channel filter not honoured")
	}
}

func TestChatChannelPayloadShape(t *testing.T) {
	clearNotifyEnv(t)
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = raw
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_CHAT_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_PUBLIC_URL", "https://lab.example.com")

	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{
		RunID: "r-chat", ScenarioID: "scn-1", OrganizationID: "default-org", ProjectID: "default-project",
		Status: "failed", VUs: 12, Error: "sla", Summary: map[string]interface{}{"p95_ms": 512.0, "error_rate": 0.2},
	}))
	if by["chat"].Status != "sent" {
		t.Fatalf("chat=%#v", by["chat"])
	}
	mu.Lock()
	defer mu.Unlock()
	var msg map[string]interface{}
	if json.Unmarshal(body, &msg) != nil {
		t.Fatalf("bad json: %s", body)
	}
	text, _ := msg["text"].(string)
	if !strings.Contains(text, "r-chat") || !strings.Contains(text, "failed") {
		t.Fatalf("text=%q", text)
	}
	if !strings.Contains(text, "512") {
		t.Fatalf("expected p95 in the message: %q", text)
	}
	if !strings.Contains(text, "https://lab.example.com") {
		t.Fatalf("expected run link: %q", text)
	}
	if _, ok := msg["attachments"]; !ok {
		t.Fatal("expected an attachments block")
	}
}

func TestChatChannelFailureIsReported(t *testing.T) {
	clearNotifyEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_CHAT_WEBHOOK_URL", srv.URL)
	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{RunID: "r-chat-fail", Status: "failed"}))
	if by["chat"].Status != "failed" || !strings.Contains(by["chat"].Detail, "503") {
		t.Fatalf("chat=%#v", by["chat"])
	}
}

func TestEmailWithoutRelayIsLoggedNotDropped(t *testing.T) {
	clearNotifyEnv(t)
	t.Setenv("OPL_RUN_EMAIL_TO", "ops@example.com, perf@example.com")
	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{RunID: "r-mail", Status: "failed"}))
	res := by["email"]
	if res.Status != "logged" {
		t.Fatalf("email=%#v want logged", res)
	}
	if !strings.Contains(res.Detail, "OPA_SMTP_HOST") {
		t.Fatalf("must name the missing relay setting: %q", res.Detail)
	}
	if res.Target != "2 recipient(s)" {
		t.Fatalf("target must count recipients without listing them: %q", res.Target)
	}
	if strings.Contains(res.Target, "@") {
		t.Fatalf("recipient addresses must not leak into the target: %q", res.Target)
	}
}

func TestEmailRelayFailureIsReported(t *testing.T) {
	clearNotifyEnv(t)
	t.Setenv("OPL_RUN_EMAIL_TO", "ops@example.com")
	// 127.0.0.1:1 refuses connections — proves a relay error surfaces as failed.
	t.Setenv("OPA_SMTP_HOST", "127.0.0.1")
	t.Setenv("OPA_SMTP_PORT", "1")
	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{RunID: "r-mail-fail", Status: "failed"}))
	if by["email"].Status != "failed" || !strings.Contains(by["email"].Detail, "SMTP send failed") {
		t.Fatalf("email=%#v", by["email"])
	}
}

func TestEmailBodyCarriesRunFacts(t *testing.T) {
	clearNotifyEnv(t)
	evt := runNotifyEvent{
		RunID: "r-body", ScenarioID: "scn-9", OrganizationID: "org", ProjectID: "proj",
		Status: "failed", VUs: 4, Source: "metrics", Error: "gate failed",
		Summary: map[string]interface{}{"p50_ms": 1.0, "p95_ms": 2.0, "p99_ms": 3.0, "error_rate": 0.5},
	}
	subject, body := runNotifyEmailBody(evt, runNotifyPayload(evt))
	if !strings.Contains(subject, "r-body") || !strings.Contains(subject, "failed") {
		t.Fatalf("subject=%q", subject)
	}
	for _, want := range []string{"scn-9", "org / proj", "gate failed", "1 / 2 / 3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestMailHeaderInjectionIsStripped(t *testing.T) {
	if got := sanitizeMailHeader("a\r\nBcc: attacker@example.com"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("CRLF survived: %q", got)
	}
}

func TestLogModeAppliesToEveryChannel(t *testing.T) {
	clearNotifyEnv(t)
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OPL_RUN_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_CHAT_WEBHOOK_URL", srv.URL)
	t.Setenv("OPL_RUN_EMAIL_TO", "ops@example.com")
	t.Setenv("OPA_SMTP_HOST", "smtp.example.com")
	t.Setenv("OPL_RUN_NOTIFY_MODE", "log")

	by := resultsByChannel(deliverRunNotifyAll(runNotifyEvent{RunID: "r-log", Status: "failed"}))
	for _, ch := range runNotifyChannels {
		if by[ch].Status != "logged" {
			t.Fatalf("%s=%#v want logged", ch, by[ch])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("log mode must not send: hits=%d", hits)
	}
}

func TestRunNotifyStatusInfoDescribesChannelsWithoutSecrets(t *testing.T) {
	clearNotifyEnv(t)
	info := runNotifyStatusInfo()
	if info["configured"] != false || info["channels_ready"] != 0 {
		t.Fatalf("%#v", info)
	}
	if !strings.Contains(info["honesty"].(string), "never silently dropped") {
		t.Fatalf("honesty=%v", info["honesty"])
	}

	t.Setenv("OPL_RUN_WEBHOOK_URL", "https://hooks.example.com/secret/path?token=abc")
	t.Setenv("OPL_RUN_WEBHOOK_SECRET", "s3cr3t")
	t.Setenv("OPL_RUN_CHAT_WEBHOOK_URL", "https://chat.example.com/services/T/B/xyz")
	t.Setenv("OPL_RUN_EMAIL_TO", "ops@example.com")
	t.Setenv("OPA_SMTP_HOST", "smtp.example.com")
	t.Setenv("OPA_SMTP_PASS", "smtp-password")
	info = runNotifyStatusInfo()
	if info["channels_ready"] != 3 {
		t.Fatalf("channels_ready=%v", info["channels_ready"])
	}
	blob, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	s := string(blob)
	for _, secret := range []string{"s3cr3t", "smtp-password", "token=abc", "/secret/path", "xyz", "ops@example.com"} {
		if strings.Contains(s, secret) {
			t.Fatalf("health payload leaked %q: %s", secret, s)
		}
	}
	channels, _ := info["channels"].([]map[string]interface{})
	if len(channels) != 3 {
		t.Fatalf("channels=%#v", channels)
	}
	for _, c := range channels {
		if c["configured"] != true {
			t.Fatalf("channel %v not reported configured", c["name"])
		}
	}
	if channels[0]["name"] != "webhook" || channels[0]["signed"] != true {
		t.Fatalf("webhook entry=%#v", channels[0])
	}
	if channels[0]["target"] != "https://hooks.example.com" {
		t.Fatalf("webhook target must be host-only: %v", channels[0]["target"])
	}
}

func TestRunNotifyStatusInfoNamesUnconfiguredChannels(t *testing.T) {
	clearNotifyEnv(t)
	t.Setenv("OPL_RUN_WEBHOOK_URL", "https://hooks.example.com/x")
	info := runNotifyStatusInfo()
	channels, _ := info["channels"].([]map[string]interface{})
	found := 0
	for _, c := range channels {
		if c["configured"] == false {
			found++
			if reason, _ := c["reason"].(string); !strings.Contains(reason, "not configured") {
				t.Fatalf("channel %v must carry a plain reason, got %q", c["name"], reason)
			}
		}
	}
	if found != 2 {
		t.Fatalf("expected chat+email reported as unconfigured, got %d", found)
	}
	if info["configured"] != true {
		t.Fatal("one ready channel means configured")
	}
}

func TestRunNotifyConfiguredAcrossChannels(t *testing.T) {
	clearNotifyEnv(t)
	if runNotifyConfigured() {
		t.Fatal("nothing configured")
	}
	t.Setenv("OPL_RUN_EMAIL_TO", "ops@example.com")
	if !runNotifyConfigured() {
		t.Fatal("email recipients alone count as configured")
	}
	t.Setenv("OPL_RUN_NOTIFY_CHANNELS", "webhook")
	if runNotifyConfigured() {
		t.Fatal("email disabled by the channel filter")
	}
}
