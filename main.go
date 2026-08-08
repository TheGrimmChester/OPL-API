package main

import (
	"log"
	"os"
	"net/http"
	"time"
)

var (
	queryClient  *ClickHouseQuery
	writer       *ClickHouseWriter
	buildVersion = "perf-lab-dev"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "orchestrator" {
		runOPLOrchestrator()
		return
	}
	addr := envOr("HTTP_ADDR", ":8092")
	chURL := envOr("CLICKHOUSE_URL", "http://127.0.0.1:8123")

	writer = NewClickHouseWriter(chURL, 100)
	queryClient = NewClickHouseQuery(chURL)
	ensureClickHouseDatabase(queryClient)
	ensurePerfLabSchema(queryClient)
	initSecurityCache()
	initFederationPeers()
	initAuthMode()
	startPerfScheduler()

	authRequired := authRequiredEnv()
	setAuthEnforced(authRequired)
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
	authEditor := func(pattern string, h http.HandlerFunc) {
		if authRequired {
			mux.HandleFunc(pattern, AuthMiddleware(h, "editor"))
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
			"status":     "ok",
			"service":    "opl-api",
			"version":    buildVersion,
			"database":   clickHouseDatabase(),
			"auth_mode":  string(authMode),
			"run_notify": runNotifyStatusInfo(),
		})
	})
	registerLocalAuthMux(mux)

	registerHubOAMMux(mux, authView)
	registerPerfLabMux(mux, authView, authEditor, authAdmin)
	registerJMeterMux(mux, authView, authEditor, authAdmin)
	registerPeerSCMMux(mux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           corsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("opl-api listening on %s (CH=%s db=%s)", addr, chURL, clickHouseDatabase())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
