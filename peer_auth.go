package main

import "net/http"

// Peer auth for SCM checker fan-out from ora-api.
//
// ORA mints service JWTs (iss=ora-api, aud=<product>-api, scope scm:events).
// Routes registered with authView accept user JWTs only and reject service tokens.

func registerPeerAuth(mux *http.ServeMux, pattern, readScope, writeScope string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !authEnforced {
			h(w, r)
			return
		}
		scope := writeScope
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			scope = readScope
		}
		AuthUserOrServiceMiddleware(h, "viewer", scope)(w, r)
	})
}
