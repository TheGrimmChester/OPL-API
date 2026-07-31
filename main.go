package main

import (
	"log"
	"net/http"
	"time"
)

var (
	queryClient  *ClickHouseQuery
	writer       *ClickHouseWriter
	buildVersion = "perf-lab-dev"
)

func main() {
	addr := envOr("HTTP_ADDR", ":8092")
	chURL := envOr("CLICKHOUSE_URL", "http://127.0.0.1:8123")

	writer = NewClickHouseWriter(chURL, 100)
	queryClient = NewClickHouseQuery(chURL)

	authRequired := authRequiredEnv()
	authEnforced = authRequired
	if authRequired {
		log.Printf("auth: ENABLED (OPA_AUTH_REQUIRED)")
	} else {
		log.Printf("auth: disabled — endpoints open")
	}

	mux := http.NewServeMux()
	authView := func(pattern string, h http.HandlerFunc) {
		if authRequired {
			mux.HandleFunc(pattern, AuthMiddleware(h, "viewer"))
		} else {
			mux.HandleFunc(pattern, h)
		}
	}
	authAdmin := func(pattern string, h http.HandlerFunc) {
		if authRequired {
			mux.HandleFunc(pattern, AuthMiddleware(h, "admin"))
		} else {
			mux.HandleFunc(pattern, h)
		}
	}

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"status":  "ok",
			"service": "opa-perf-lab",
			"version": buildVersion,
		})
	})

	registerWave29Mux(mux, authView, authAdmin)
	registerWave31Mux(mux, authView, authAdmin)

	srv := &http.Server{
		Addr:              addr,
		Handler:           corsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("OPA Perf Lab listening on %s (CH=%s)", addr, chURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
