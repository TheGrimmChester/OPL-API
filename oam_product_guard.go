package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// requireEnabledOAMProject rejects when PEER_OAM_URL is set and the concrete
// X-Project-ID is absent from GET OAM /api/projects?product=<code>.
// Skip when PEER_OAM_URL is unset, or the project header is empty/"all".
func requireEnabledOAMProject(r *http.Request, product string) (status int, msg string) {
	base := peerOAMURL()
	if base == "" {
		return 0, ""
	}
	proj := strings.TrimSpace(r.Header.Get("X-Project-ID"))
	if proj == "" || strings.EqualFold(proj, "all") {
		return 0, ""
	}
	ok, err := oamDirectoryHasProject(r.Context(), r, base, product, proj)
	if err != nil {
		return 503, "could not verify project enablement with OAM: " + err.Error()
	}
	if !ok {
		return 403, fmt.Sprintf("project %q is disabled for product %q (OAM disabled_products)", proj, product)
	}
	return 0, ""
}

func oamDirectoryHasProject(ctx context.Context, r *http.Request, base, product, projectID string) (bool, error) {
	q := url.Values{}
	q.Set("product", product)
	if org := strings.TrimSpace(r.Header.Get("X-Organization-ID")); org != "" && !strings.EqualFold(org, "all") {
		q.Set("organization_id", org)
	}
	target := oamProjectsTarget(base, q)
	raw, status, err := proxyPeerGET(ctx, target, r)
	if err != nil {
		return false, err
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("oam returned %d", status)
	}
	var payload struct {
		Projects []struct {
			ID        string `json:"id"`
			ProjectID string `json:"project_id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, err
	}
	for _, row := range payload.Projects {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			id = strings.TrimSpace(row.ProjectID)
		}
		if id == projectID {
			return true, nil
		}
	}
	return false, nil
}
