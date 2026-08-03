package main

import (
	"log"
	"net/http"

	openjob "github.com/TheGrimmChester/open-job-go"
)

func runOPLOrchestrator() {
	addr := envOr("ORCHESTRATOR_LISTEN_ADDR", ":8097")
	tag := envOr("OPL_RUNNER_TAG", "smoke")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"status":  "ok",
			"service": "opl-orchestrator",
			"version": buildVersion,
			"runners": []string{openjob.RunnerImage("opl", "jmeter", tag)},
		})
	})
	log.Printf("opl-orchestrator listening on %s (one container per load run)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
