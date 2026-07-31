package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func getString(row map[string]interface{}, key string) string {
	if row == nil {
		return ""
	}
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func getFloat64(row map[string]interface{}, key string) float64 {
	if row == nil {
		return 0
	}
	v, ok := row[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	default:
		return 0
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func truncateStr(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// Federation stubs — fan-out peers live on Agent; Perf Lab runs locally / Docker JMeter.
type federationPeer struct {
	ID      string `json:"id"`
	Region  string `json:"region"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
	Notes   string `json:"notes"`
}

func localAgentRegion() string { return envOr("OPA_REGION", "local") }

func federationPeersSnapshot() []federationPeer { return nil }

func enforceWriteLocalityHTTP(w http.ResponseWriter, r *http.Request, org, proj string) bool {
	_ = w
	_ = r
	_ = org
	_ = proj
	return true
}

func runLocalLoadSample(target, method string, vus, durSec int, loadRunID string, simulate bool) map[string]interface{} {
	_ = target
	_ = method
	_ = vus
	_ = durSec
	_ = loadRunID
	_ = simulate
	return map[string]interface{}{
		"ok":      false,
		"skipped": true,
		"honesty": "Federation local sample runner stays on Agent; Perf Lab uses Docker JMeter dispatch.",
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Organization-ID, X-Project-ID")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var _ = time.Time{}


func nz(s, d string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return d
}
