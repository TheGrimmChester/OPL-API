package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	openauth "github.com/TheGrimmChester/open-auth-go"
)

var (
	authMode     openauth.Mode
	localIssuer  *openauth.LocalIssuer
	authIssuerID = "opl-api"
)

func initAuthMode() {
	authMode = openauth.ResolveMode(os.Getenv("AUTH_MODE"), os.Getenv("PEER_OPA_URL"))
	if openauth.IsStandalone(authMode) {
		localIssuer = openauth.NewLocalIssuer(jwtSecret, authIssuerID, envOr("AUTH_ADMIN_USER", "admin"), envOr("AUTH_ADMIN_PASSWORD", "admin"))
		// Prefer the issuer secret (may be ephemeral) as the process jwtSecret.
		jwtSecret = localIssuer.Secret
		log.Printf("auth: mode=standalone issuer=%s (local /api/auth/login)", authIssuerID)
		return
	}
	log.Printf("auth: mode=codeployed (validate shared JWT_SECRET; hub issues tokens)")
}

func registerLocalAuthMux(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/login", serveAuthLogin)
	mux.HandleFunc("/api/auth/status", serveAuthStatus)
	mux.HandleFunc("/api/auth/logout", serveAuthLogout)
	if openauth.IsStandalone(authMode) {
		mux.HandleFunc("/api/auth/register", serveAuthRegister)
	}
}

func serveAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !openauth.IsStandalone(authMode) || localIssuer == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "auth_mode",
			"message": "login is issued by OPA-Hub in co-deployed mode; set AUTH_MODE=standalone for local auth",
			"mode":    string(authMode),
		})
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&creds); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	tok, exp, claims, err := localIssuer.Login(creds.Username, creds.Password)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]interface{}{
		"token":      tok,
		"expires_at": exp.Format("2006-01-02T15:04:05Z07:00"),
		"mode":       string(authMode),
		"user":       map[string]interface{}{"username": claims.Username, "role": claims.Role},
	})
}

func serveAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out := map[string]interface{}{
		"mode":          string(authMode),
		"auth_required": authRequiredEnv(),
		"issuer":        authIssuerID,
		"standalone":    openauth.IsStandalone(authMode),
	}
	tok := bearerOrCookie(r)
	if tok != "" {
		if claims, err := openauth.ParseUserJWT(tok, jwtSecret); err == nil {
			out["authenticated"] = true
			out["user"] = map[string]interface{}{"username": claims.Username, "role": claims.Role}
			writeJSON(w, out)
			return
		}
	}
	out["authenticated"] = false
	writeJSON(w, out)
}

func serveAuthLogout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"ok": true})
}

func serveAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if localIssuer == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if err := localIssuer.Register(body.Username, body.Password, body.Role); err != nil {
		http.Error(w, "conflict or weak credentials", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":   true,
		"user": map[string]interface{}{"username": body.Username, "role": body.Role},
	})
}

func bearerOrCookie(r *http.Request) string {
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return c.Value
	}
	return ""
}
