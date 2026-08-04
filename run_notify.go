package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Terminal-run webhook notifications.
//
// Env:
//
//	OPL_RUN_WEBHOOK_URL     — HTTPS/HTTP endpoint; empty disables delivery
//	OPL_RUN_NOTIFY_MODE     — deliver (default) | log | log-only | dry-run
//	OPL_RUN_NOTIFY_STATUSES — comma list of statuses to notify (default: all terminal)
//	OPL_RUN_WEBHOOK_SECRET  — optional; sent as X-OPL-Signature: sha256=<hmac>
//	OPL_PUBLIC_URL          — optional link base included in payload

var (
	runNotifyHTTPClient = &http.Client{Timeout: 12 * time.Second}
	runNotifyOnceDedup  sync.Map // runID|status → struct{} to avoid double-fire within process
)

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

func runNotifyConfigured() bool {
	return runNotifyWebhookURL() != ""
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

// runNotifyStatusInfo is safe for /api/health (no secrets / full URL).
func runNotifyStatusInfo() map[string]interface{} {
	url := runNotifyWebhookURL()
	info := map[string]interface{}{
		"configured": url != "",
		"mode":       runNotifyMode(),
	}
	if raw := strings.TrimSpace(os.Getenv("OPL_RUN_NOTIFY_STATUSES")); raw != "" {
		info["statuses"] = raw
	} else {
		info["statuses"] = "terminal"
	}
	if url != "" {
		info["url_host"] = redactWebhookHost(url)
	}
	if strings.TrimSpace(os.Getenv("OPL_RUN_WEBHOOK_SECRET")) != "" {
		info["signed"] = true
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
		status := deliverRunNotify(evt)
		log.Printf("[run-notify] run=%s status=%s source=%s result=%s", evt.RunID, evt.Status, evt.Source, status)
	}()
}

func deliverRunNotify(evt runNotifyEvent) string {
	url := runNotifyWebhookURL()
	mode := runNotifyMode()
	if url == "" {
		return "skipped"
	}
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
	body, err := json.Marshal(payload)
	if err != nil {
		return "failed"
	}
	if mode == "log" {
		log.Printf("[run-notify] log mode url=%s payload=%s", redactWebhookHost(url), string(body))
		return "logged"
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "failed"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opl-api-run-notify/1")
	if secret := strings.TrimSpace(os.Getenv("OPL_RUN_WEBHOOK_SECRET")); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		req.Header.Set("X-OPL-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := runNotifyHTTPClient.Do(req)
	if err != nil {
		log.Printf("[run-notify] POST failed: %v", err)
		return "failed"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		log.Printf("[run-notify] webhook HTTP %d", resp.StatusCode)
		return "failed"
	}
	return "sent"
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
