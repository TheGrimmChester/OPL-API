package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"
)

// Terminal-run notifications across channels.
//
// Shared controls (apply to every channel):
//
//	OPL_RUN_NOTIFY_MODE     — deliver (default) | log | log-only | dry-run
//	OPL_RUN_NOTIFY_STATUSES — comma list of statuses to notify (default: all terminal)
//	OPL_RUN_NOTIFY_CHANNELS — comma list to restrict channels (webhook,chat,email); empty = all
//	OPL_PUBLIC_URL          — optional link base included in payloads
//
// Channel: webhook (raw JSON POST)
//
//	OPL_RUN_WEBHOOK_URL     — endpoint; empty leaves the channel unconfigured
//	OPL_RUN_WEBHOOK_SECRET  — optional; sent as X-OPL-Signature: sha256=<hmac>
//
// Channel: chat (chat-platform incoming webhook; message payload with a text field)
//
//	OPL_RUN_CHAT_WEBHOOK_URL — incoming webhook URL; empty leaves the channel unconfigured
//
// Channel: email (SMTP; shares the stack SMTP block with the edge agent)
//
//	OPL_RUN_EMAIL_TO   — comma list of recipients; empty leaves the channel unconfigured
//	OPL_RUN_EMAIL_FROM — optional From override (defaults to OPA_SMTP_FROM / OPA_SMTP_USER)
//	OPA_SMTP_HOST / OPA_SMTP_PORT / OPA_SMTP_USER / OPA_SMTP_PASS / OPA_SMTP_FROM
//
// Unconfigured channels are never silently dropped: they are reported in
// /api/health and recorded in the notification history with status "skipped"
// and a plain reason.

var (
	runNotifyHTTPClient = &http.Client{Timeout: 12 * time.Second}
	runNotifyOnceDedup  sync.Map // runID|status → struct{} to avoid double-fire within process
)

// runNotifyChannels is the fixed set of delivery channels, in report order.
var runNotifyChannels = []string{"webhook", "chat", "email"}

type runNotifyEvent struct {
	RunID          string                 `json:"run_id"`
	ScenarioID     string                 `json:"scenario_id,omitempty"`
	OrganizationID string                 `json:"organization_id,omitempty"`
	ProjectID      string                 `json:"project_id,omitempty"`
	Status         string                 `json:"status"`
	VUs            int                    `json:"vus,omitempty"`
	Error          string                 `json:"error,omitempty"`
	Summary        map[string]interface{} `json:"summary,omitempty"`
	FinishedAt     string                 `json:"finished_at,omitempty"`
	Source         string                 `json:"source,omitempty"` // metrics|jmeter|cancel|dispatch|import-jtl|scheduler
}

// runNotifyResult is one channel attempt, persisted to the notification history.
type runNotifyResult struct {
	Channel string `json:"channel"`
	Status  string `json:"status"` // sent|failed|logged|skipped
	Target  string `json:"target"` // redacted destination (never credentials)
	Detail  string `json:"detail"`
}

func runNotifyMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OPL_RUN_NOTIFY_MODE")))
	switch v {
	case "", "deliver", "send":
		return "deliver"
	case "log", "log-only", "dry-run", "dryrun":
		return "log"
	default:
		return "deliver"
	}
}

func runNotifyWebhookURL() string {
	return strings.TrimSpace(os.Getenv("OPL_RUN_WEBHOOK_URL"))
}

func runNotifyChatWebhookURL() string {
	return strings.TrimSpace(os.Getenv("OPL_RUN_CHAT_WEBHOOK_URL"))
}

// runNotifyEmailRecipients splits OPL_RUN_EMAIL_TO into a clean recipient list.
func runNotifyEmailRecipients() []string {
	raw := strings.TrimSpace(os.Getenv("OPL_RUN_EMAIL_TO"))
	if raw == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func runNotifySMTPHost() string {
	return strings.TrimSpace(os.Getenv("OPA_SMTP_HOST"))
}

func runNotifySMTPPort() string {
	if v := strings.TrimSpace(os.Getenv("OPA_SMTP_PORT")); v != "" {
		return v
	}
	return "587"
}

func runNotifyStatusesFilter() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("OPL_RUN_NOTIFY_STATUSES"))
	if raw == "" || raw == "*" || strings.EqualFold(raw, "all") || strings.EqualFold(raw, "terminal") {
		return nil // nil = all terminal
	}
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		s := strings.ToLower(strings.TrimSpace(p))
		if s != "" {
			out[s] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runNotifyChannelsFilter restricts which channels are attempted. nil = all.
func runNotifyChannelsFilter() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("OPL_RUN_NOTIFY_CHANNELS"))
	if raw == "" || raw == "*" || strings.EqualFold(raw, "all") {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		s := strings.ToLower(strings.TrimSpace(p))
		if s != "" {
			out[s] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runNotifyChannelEnabled(name string) bool {
	f := runNotifyChannelsFilter()
	if f == nil {
		return true
	}
	return f[name]
}

// runNotifyChannelConfigured reports whether a channel has a destination, plus
// the redacted target and the plain reason when it does not.
func runNotifyChannelConfigured(name string) (bool, string, string) {
	switch name {
	case "webhook":
		if u := runNotifyWebhookURL(); u != "" {
			return true, redactWebhookHost(u), ""
		}
		return false, "", "webhook channel not configured (OPL_RUN_WEBHOOK_URL unset)"
	case "chat":
		if u := runNotifyChatWebhookURL(); u != "" {
			return true, redactWebhookHost(u), ""
		}
		return false, "", "chat channel not configured (OPL_RUN_CHAT_WEBHOOK_URL unset)"
	case "email":
		rcpts := runNotifyEmailRecipients()
		if len(rcpts) == 0 {
			return false, "", "email channel not configured (OPL_RUN_EMAIL_TO unset)"
		}
		target := fmt.Sprintf("%d recipient(s)", len(rcpts))
		host := runNotifySMTPHost()
		if host == "" {
			// Recipients without a relay: honest partial config — recorded, not sent.
			return true, target, "SMTP relay not configured (OPA_SMTP_HOST unset) — email is recorded, not sent"
		}
		return true, host + ":" + runNotifySMTPPort() + " → " + target, ""
	}
	return false, "", "unknown channel"
}

// runNotifyConfigured is true when at least one enabled channel has a destination.
func runNotifyConfigured() bool {
	for _, ch := range runNotifyChannels {
		if !runNotifyChannelEnabled(ch) {
			continue
		}
		if ok, _, _ := runNotifyChannelConfigured(ch); ok {
			return true
		}
	}
	return false
}

func runNotifyStatusAllowed(status string) bool {
	st := strings.ToLower(strings.TrimSpace(status))
	if !runStatusTerminal(st) {
		return false
	}
	filter := runNotifyStatusesFilter()
	if filter == nil {
		return true
	}
	return filter[st]
}

// runNotifyStatusInfo is safe for /api/health (no secrets, no full URLs, no recipients).
func runNotifyStatusInfo() map[string]interface{} {
	signed := strings.TrimSpace(os.Getenv("OPL_RUN_WEBHOOK_SECRET")) != ""
	channels := make([]map[string]interface{}, 0, len(runNotifyChannels))
	ready := 0
	for _, name := range runNotifyChannels {
		enabled := runNotifyChannelEnabled(name)
		ok, target, reason := runNotifyChannelConfigured(name)
		entry := map[string]interface{}{
			"name":       name,
			"enabled":    enabled,
			"configured": ok,
		}
		if target != "" {
			entry["target"] = target
		}
		if reason != "" {
			entry["reason"] = reason
		}
		if name == "webhook" && signed {
			entry["signed"] = true
		}
		if !enabled {
			entry["reason"] = "channel disabled by OPL_RUN_NOTIFY_CHANNELS"
		}
		if ok && enabled {
			ready++
		}
		channels = append(channels, entry)
	}
	info := map[string]interface{}{
		"configured":         ready > 0,
		"mode":               runNotifyMode(),
		"channels":           channels,
		"channels_available": len(runNotifyChannels),
		"channels_ready":     ready,
	}
	if raw := strings.TrimSpace(os.Getenv("OPL_RUN_NOTIFY_STATUSES")); raw != "" {
		info["statuses"] = raw
	} else {
		info["statuses"] = "terminal"
	}
	if url := runNotifyWebhookURL(); url != "" {
		info["url_host"] = redactWebhookHost(url)
	}
	if signed {
		info["signed"] = true
	}
	if ready == 0 {
		info["honesty"] = "No delivery channel configured — terminal runs are recorded in the notification history as skipped, never silently dropped."
	} else {
		info["honesty"] = "Configured channels are attempted on terminal run status; every attempt (sent/failed/logged/skipped) is recorded in the notification history."
	}
	return info
}

func redactWebhookHost(raw string) string {
	// Keep scheme+host only; drop path/query (may contain tokens).
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return raw[:i+3] + rest
	}
	if j := strings.IndexAny(raw, "/?#"); j >= 0 {
		return raw[:j]
	}
	return raw
}

// notifyRunTerminal fires asynchronously when a run reaches an allowed terminal status.
// Safe to call from handlers and engine goroutines; never blocks the caller on HTTP.
func notifyRunTerminal(evt runNotifyEvent) {
	evt.Status = strings.ToLower(strings.TrimSpace(evt.Status))
	if !runNotifyStatusAllowed(evt.Status) {
		return
	}
	key := evt.RunID + "|" + evt.Status
	if evt.RunID != "" {
		if _, loaded := runNotifyOnceDedup.LoadOrStore(key, struct{}{}); loaded {
			return
		}
	}
	go func() {
		results := deliverRunNotifyAll(evt)
		for _, res := range results {
			log.Printf("[run-notify] run=%s status=%s source=%s channel=%s result=%s %s",
				evt.RunID, evt.Status, evt.Source, res.Channel, res.Status, res.Detail)
		}
		recordRunNotifyHistory(evt, results)
	}()
}

// runNotifyPayload builds the canonical JSON event shared by every channel.
func runNotifyPayload(evt runNotifyEvent) map[string]interface{} {
	payload := map[string]interface{}{
		"event":           "opl.run.terminal",
		"service":         "opl-api",
		"run_id":          evt.RunID,
		"scenario_id":     evt.ScenarioID,
		"organization_id": evt.OrganizationID,
		"project_id":      evt.ProjectID,
		"status":          evt.Status,
		"vus":             evt.VUs,
		"error":           evt.Error,
		"source":          evt.Source,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	}
	if evt.FinishedAt != "" {
		payload["finished_at"] = evt.FinishedAt
	}
	if evt.Summary != nil {
		payload["summary"] = evt.Summary
	}
	if pub := strings.TrimSpace(os.Getenv("OPL_PUBLIC_URL")); pub != "" {
		payload["run_url"] = strings.TrimRight(pub, "/") + "/?tab=results&run=" + evt.RunID
	}
	return payload
}

// deliverRunNotifyAll attempts every channel and returns one result per channel.
// Channels that are disabled or unconfigured yield "skipped" with a plain reason.
func deliverRunNotifyAll(evt runNotifyEvent) []runNotifyResult {
	payload := runNotifyPayload(evt)
	body, encErr := json.Marshal(payload)
	mode := runNotifyMode()
	out := make([]runNotifyResult, 0, len(runNotifyChannels))
	for _, name := range runNotifyChannels {
		res := runNotifyResult{Channel: name}
		if !runNotifyChannelEnabled(name) {
			res.Status = "skipped"
			res.Detail = "channel disabled by OPL_RUN_NOTIFY_CHANNELS"
			out = append(out, res)
			continue
		}
		ok, target, reason := runNotifyChannelConfigured(name)
		res.Target = target
		if !ok {
			res.Status = "skipped"
			res.Detail = reason
			out = append(out, res)
			continue
		}
		if encErr != nil {
			res.Status = "failed"
			res.Detail = "payload encode failed"
			out = append(out, res)
			continue
		}
		if mode == "log" {
			res.Status = "logged"
			res.Detail = "mode=log — intentional no-send"
			log.Printf("[run-notify] log mode channel=%s target=%s payload=%s", name, target, string(body))
			out = append(out, res)
			continue
		}
		switch name {
		case "webhook":
			res.Status, res.Detail = sendRunNotifyWebhook(body)
		case "chat":
			res.Status, res.Detail = sendRunNotifyChat(evt, payload)
		case "email":
			res.Status, res.Detail = sendRunNotifyEmail(evt, payload, reason)
		}
		out = append(out, res)
	}
	return out
}

// deliverRunNotify keeps the single-status contract of the raw webhook channel.
func deliverRunNotify(evt runNotifyEvent) string {
	if runNotifyWebhookURL() == "" || !runNotifyChannelEnabled("webhook") {
		return "skipped"
	}
	body, err := json.Marshal(runNotifyPayload(evt))
	if err != nil {
		return "failed"
	}
	if runNotifyMode() == "log" {
		log.Printf("[run-notify] log mode url=%s payload=%s", redactWebhookHost(runNotifyWebhookURL()), string(body))
		return "logged"
	}
	st, _ := sendRunNotifyWebhook(body)
	return st
}

func sendRunNotifyWebhook(body []byte) (string, string) {
	url := runNotifyWebhookURL()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "failed", "request build failed"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opl-api-run-notify/1")
	signed := false
	if secret := strings.TrimSpace(os.Getenv("OPL_RUN_WEBHOOK_SECRET")); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		req.Header.Set("X-OPL-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		signed = true
	}
	resp, err := runNotifyHTTPClient.Do(req)
	if err != nil {
		log.Printf("[run-notify] webhook POST failed: %v", err)
		return "failed", "POST failed: " + redactWebhookHost(url)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		log.Printf("[run-notify] webhook HTTP %d", resp.StatusCode)
		return "failed", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if signed {
		detail += " (signed)"
	}
	return "sent", detail
}

// runNotifyChatMessage renders the chat incoming-webhook message payload
// (a text field plus a colored attachment) from the canonical event.
func runNotifyChatMessage(evt runNotifyEvent, payload map[string]interface{}) map[string]interface{} {
	label := "OK"
	switch evt.Status {
	case "failed", "error", "aborted":
		label = "FAILED"
	case "cancelled", "canceled":
		label = "CANCELLED"
	}
	lines := []string{
		fmt.Sprintf("*Load run %s* — %s", label, evt.Status),
		fmt.Sprintf("run: `%s`", evt.RunID),
	}
	if evt.ScenarioID != "" {
		lines = append(lines, fmt.Sprintf("scenario: `%s`", evt.ScenarioID))
	}
	if evt.OrganizationID != "" || evt.ProjectID != "" {
		lines = append(lines, fmt.Sprintf("scope: %s / %s", nz(evt.OrganizationID, "-"), nz(evt.ProjectID, "-")))
	}
	if evt.VUs > 0 {
		lines = append(lines, fmt.Sprintf("VUs: %d", evt.VUs))
	}
	if evt.Summary != nil {
		lines = append(lines, fmt.Sprintf("p95: %gms · error rate: %g",
			numFrom(evt.Summary, "p95_ms"), numFrom(evt.Summary, "error_rate")))
	}
	if evt.Error != "" {
		lines = append(lines, "error: "+evt.Error)
	}
	if u, ok := payload["run_url"].(string); ok && u != "" {
		lines = append(lines, u)
	}
	text := strings.Join(lines, "\n")
	return map[string]interface{}{
		"text": text,
		"attachments": []map[string]interface{}{{
			"text":  text,
			"color": chatColorForStatus(evt.Status),
		}},
	}
}

func chatColorForStatus(status string) string {
	switch status {
	case "failed", "error", "aborted":
		return "#d94f4f"
	case "cancelled", "canceled":
		return "#c9a227"
	default:
		return "#2f9e6e"
	}
}

func sendRunNotifyChat(evt runNotifyEvent, payload map[string]interface{}) (string, string) {
	url := runNotifyChatWebhookURL()
	body, err := json.Marshal(runNotifyChatMessage(evt, payload))
	if err != nil {
		return "failed", "payload encode failed"
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "failed", "request build failed"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opl-api-run-notify/1")
	resp, err := runNotifyHTTPClient.Do(req)
	if err != nil {
		log.Printf("[run-notify] chat POST failed: %v", err)
		return "failed", "POST failed: " + redactWebhookHost(url)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		log.Printf("[run-notify] chat webhook HTTP %d", resp.StatusCode)
		return "failed", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return "sent", fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// sendRunNotifyEmail relays the event over SMTP. Recipients without a relay are
// recorded as "logged" (intentional no-send) rather than dropped.
func sendRunNotifyEmail(evt runNotifyEvent, payload map[string]interface{}, partialReason string) (string, string) {
	recipients := runNotifyEmailRecipients()
	if len(recipients) == 0 {
		return "skipped", "email channel not configured (OPL_RUN_EMAIL_TO unset)"
	}
	subject, body := runNotifyEmailBody(evt, payload)
	host := runNotifySMTPHost()
	if host == "" {
		log.Printf("[run-notify] email recipients=%d subject=%q (OPA_SMTP_HOST not set; logged only)", len(recipients), subject)
		return "logged", partialReason
	}
	port := runNotifySMTPPort()
	user := strings.TrimSpace(os.Getenv("OPA_SMTP_USER"))
	pass := os.Getenv("OPA_SMTP_PASS")
	from := strings.TrimSpace(os.Getenv("OPL_RUN_EMAIL_FROM"))
	if from == "" {
		from = strings.TrimSpace(os.Getenv("OPA_SMTP_FROM"))
	}
	if from == "" {
		from = user
	}
	if from == "" {
		from = "opl-api@localhost"
	}
	var msg bytes.Buffer
	now := time.Now().UTC()
	fmt.Fprintf(&msg, "From: %s\r\n", sanitizeMailHeader(from))
	fmt.Fprintf(&msg, "To: %s\r\n", sanitizeMailHeader(strings.Join(recipients, ", ")))
	fmt.Fprintf(&msg, "Subject: %s\r\n", sanitizeMailHeader(subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", now.Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	msg.WriteString(body)

	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	addr := net.JoinHostPort(host, port)
	if err := smtp.SendMail(addr, auth, from, recipients, msg.Bytes()); err != nil {
		log.Printf("[run-notify] email send failed via %s: %v", addr, err)
		return "failed", "SMTP send failed via " + addr
	}
	return "sent", fmt.Sprintf("SMTP %s → %d recipient(s)", addr, len(recipients))
}

func runNotifyEmailBody(evt runNotifyEvent, payload map[string]interface{}) (subject, body string) {
	subject = fmt.Sprintf("[OPL] Load run %s: %s", evt.Status, evt.RunID)
	var b strings.Builder
	fmt.Fprintf(&b, "Run:       %s\r\n", evt.RunID)
	fmt.Fprintf(&b, "Status:    %s\r\n", evt.Status)
	if evt.ScenarioID != "" {
		fmt.Fprintf(&b, "Scenario:  %s\r\n", evt.ScenarioID)
	}
	fmt.Fprintf(&b, "Scope:     %s / %s\r\n", nz(evt.OrganizationID, "-"), nz(evt.ProjectID, "-"))
	if evt.VUs > 0 {
		fmt.Fprintf(&b, "VUs:       %d\r\n", evt.VUs)
	}
	if evt.Source != "" {
		fmt.Fprintf(&b, "Source:    %s\r\n", evt.Source)
	}
	if evt.FinishedAt != "" {
		fmt.Fprintf(&b, "Finished:  %s\r\n", evt.FinishedAt)
	}
	if evt.Summary != nil {
		fmt.Fprintf(&b, "p50/p95/p99 ms: %g / %g / %g\r\n",
			numFrom(evt.Summary, "p50_ms"), numFrom(evt.Summary, "p95_ms"), numFrom(evt.Summary, "p99_ms"))
		fmt.Fprintf(&b, "Error rate:     %g\r\n", numFrom(evt.Summary, "error_rate"))
	}
	if evt.Error != "" {
		fmt.Fprintf(&b, "Error:     %s\r\n", evt.Error)
	}
	if u, ok := payload["run_url"].(string); ok && u != "" {
		fmt.Fprintf(&b, "\r\n%s\r\n", u)
	}
	return subject, b.String()
}

// sanitizeMailHeader strips CR/LF so an interpolated value cannot inject headers.
func sanitizeMailHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// parseSummaryLoose turns summary_json string or map into a map for notify payloads.
func parseSummaryLoose(v interface{}) map[string]interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return t
	case string:
		if strings.TrimSpace(t) == "" || t == "{}" {
			return nil
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(t), &m) == nil {
			return m
		}
	}
	return nil
}
