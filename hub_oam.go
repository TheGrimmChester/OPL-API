package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	openauth "github.com/TheGrimmChester/open-auth-go"
)

// registerHubOAMMux exposes hub tenancy + OAM project directory for the dashboard.
func registerHubOAMMux(mux *http.ServeMux, authView func(string, http.HandlerFunc)) {
	authView("/api/hub/organizations", handleHubOrganizations)
	authView("/api/oam/projects", handleOAMProjects)
	authView("/api/oam/organizations", handleOAMOrganizations)
}

func peerOPAURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("PEER_OPA_URL")), "/")
}

func peerOAMURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("PEER_OAM_URL")), "/")
}

func oamDirectoryConfigured() bool {
	return peerOAMURL() != ""
}

func handleHubOrganizations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	base := peerOPAURL()
	if base == "" {
		writeJSON(w, map[string]interface{}{
			"organizations":    []interface{}{},
			"peer_unavailable": true,
			"peer":             "opa-hub",
			"note":             "Set PEER_OPA_URL to discover hub organizations.",
		})
		return
	}
	raw, status, err := proxyPeerGET(r.Context(), base+"/api/tenancy/organizations", r)
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"organizations": []interface{}{},
			"error":         err.Error(),
			"peer":          "opa-hub",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(aliasDirectoryIDs(raw, "organizations", "org_id"))
}

func handleOAMOrganizations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	base := peerOAMURL()
	if base == "" {
		writeJSON(w, map[string]interface{}{
			"organizations":    []interface{}{},
			"peer_unavailable": true,
			"peer":             "oam-api",
			"note":             "Set PEER_OAM_URL to discover OAM organizations.",
		})
		return
	}
	raw, status, err := proxyPeerGET(r.Context(), base+"/api/organizations", r)
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"organizations": []interface{}{},
			"error":         err.Error(),
			"peer":          "oam-api",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(aliasDirectoryIDs(raw, "organizations", "org_id"))
}

func handleOAMProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	base := peerOAMURL()
	if base == "" {
		writeJSON(w, map[string]interface{}{
			"projects":         []interface{}{},
			"peer_unavailable": true,
			"peer":             "oam-api",
			"note":             "Set PEER_OAM_URL to discover projects.",
		})
		return
	}
	target := base + "/api/projects"
	if org := strings.TrimSpace(r.URL.Query().Get("organization_id")); org != "" && !strings.EqualFold(org, "all") {
		target += "?organization_id=" + url.QueryEscape(org)
	}
	raw, status, err := proxyPeerGET(r.Context(), target, r)
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"projects": []interface{}{},
			"error":    err.Error(),
			"peer":     "oam-api",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(aliasDirectoryIDs(raw, "projects", "project_id"))
}

func aliasDirectoryIDs(raw []byte, listKey, aliasKey string) []byte {
	var payload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return raw
	}
	list, ok := payload[listKey].([]interface{})
	if !ok {
		return raw
	}
	for _, item := range list {
		row, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if _, exists := row[aliasKey]; exists {
			continue
		}
		if id, ok := row["id"].(string); ok && id != "" {
			row[aliasKey] = id
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return out
}

func proxyPeerGET(ctx context.Context, url string, r *http.Request) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	} else if c, err := r.Cookie(openauth.CookieName); err == nil && c.Value != "" {
		req.Header.Set("Authorization", "Bearer "+c.Value)
	}
	for _, h := range []string{"X-Organization-ID", "X-Project-ID", "X-Tenant-User-ID"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}
