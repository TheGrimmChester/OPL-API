package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

const jwtSecretPlaceholder = "change-this-secret-key-in-production"
const authCookieName = "opa_token"

var jwtSecret []byte
var jwtSecretEphemeral bool

func authRequiredEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPA_AUTH_REQUIRED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func init() {
	secret := os.Getenv("JWT_SECRET")
	if secret != "" && secret != jwtSecretPlaceholder && len(secret) >= 32 {
		jwtSecret = []byte(secret)
		jwtSecretEphemeral = false
		return
	}
	if authRequiredEnv() {
		log.Fatalf("auth: OPA_AUTH_REQUIRED is set but JWT_SECRET is missing/placeholder/<32 bytes")
	}
	jwtSecret = make([]byte, 32)
	if _, err := rand.Read(jwtSecret); err != nil {
		log.Fatalf("failed to generate ephemeral JWT secret: %v", err)
	}
	jwtSecretEphemeral = true
	log.Printf("auth: JWT_SECRET unset/weak — using ephemeral secret (tokens reset on restart)")
}

type Claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

type AuthHandler struct {
	queryClient *ClickHouseQuery
}

func (ah *AuthHandler) VerifyToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token")
}

func AuthMiddleware(handler http.HandlerFunc, requiredRole string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			parts := strings.Split(authHeader, " ")
			if len(parts) != 2 || parts[0] != "Bearer" {
				http.Error(w, "invalid authorization header", 401)
				return
			}
			token = parts[1]
		} else if c, err := r.Cookie(authCookieName); err == nil {
			token = c.Value
		}
		if token == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		ah := &AuthHandler{queryClient: queryClient}
		claims, err := ah.VerifyToken(token)
		if err != nil {
			http.Error(w, "invalid token", 401)
			return
		}
		if requiredRole != "" && !hasPermission(claims.Role, requiredRole) {
			http.Error(w, "forbidden", 403)
			return
		}
		r.Header.Set("X-User-Username", claims.Username)
		r.Header.Set("X-User-Role", claims.Role)
		handler(w, r)
	}
}

func hasPermission(userRole, requiredRole string) bool {
	roleHierarchy := map[string]int{"viewer": 1, "editor": 2, "admin": 3}
	return roleHierarchy[userRole] >= roleHierarchy[requiredRole]
}

