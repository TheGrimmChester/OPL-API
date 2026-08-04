package main

import (
	"log"
	"net/http"

	openauth "github.com/TheGrimmChester/open-auth-go"
)

// Thin product wiring around Open-Auth-Go. No local JWT/middleware copies.

var (
	authGate *openauth.Gate
	authMode openauth.Mode
)

func authRequiredEnv() bool { return openauth.AuthRequiredEnv() }

func initAuthMode() {
	g, err := openauth.BootstrapFromEnv("opl-api", "opl-api")
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	authGate = g
	authMode = g.Mode
}

func registerLocalAuthMux(mux *http.ServeMux) {
	if authGate != nil {
		authGate.RegisterLocalAuth(mux)
	}
}

func AuthMiddleware(handler http.HandlerFunc, requiredRole string) http.HandlerFunc {
	return authGate.Middleware(requiredRole, handler)
}

func hasPermission(userRole, requiredRole string) bool {
	return openauth.HasPermission(userRole, requiredRole)
}
