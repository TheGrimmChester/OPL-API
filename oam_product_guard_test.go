package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireEnabledOAMProjectSkipsWhenUnset(t *testing.T) {
	t.Setenv("PEER_OAM_URL", "")
	req := httptest.NewRequest(http.MethodPost, "/api/perf/runs", nil)
	req.Header.Set("X-Project-ID", "checkout-api")
	if st, msg := requireEnabledOAMProject(req, "opl"); st != 0 || msg != "" {
		t.Fatalf("expected skip, got %d %q", st, msg)
	}
}

func TestOAMDirectoryHasProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"projects": []map[string]string{{"id": "lab"}},
		})
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ok, err := oamDirectoryHasProject(req.Context(), req, srv.URL, "opl", "lab")
	if err != nil || !ok {
		t.Fatalf("want found, got ok=%v err=%v", ok, err)
	}
}
