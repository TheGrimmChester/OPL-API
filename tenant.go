package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	opentenant "github.com/TheGrimmChester/open-tenant-go"
)

// TenantContext is the product alias for the shared Open-Tenant-Go context.
type TenantContext = opentenant.Context

const (
	tenantAll        = opentenant.All
	defaultOrgID     = opentenant.DefaultOrganizationID
	defaultProjectID = opentenant.DefaultProjectID
)

// authEnforced mirrors AUTH_REQUIRED / OPA_AUTH_REQUIRED. Keep in sync with
// opentenant via setAuthEnforced so SQL scoping matches the auth gate.
var authEnforced bool

func setAuthEnforced(v bool) {
	authEnforced = v
	opentenant.SetAuthEnforced(v)
}

// ExtractTenantContext resolves org/project from DSN / API key / headers / query.
// Hub directory tables stay on opa.* via hubTable (no legacy rewrite).
func ExtractTenantContext(r *http.Request, queryClient *ClickHouseQuery) (*TenantContext, error) {
	ctx := &TenantContext{}

	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		if strings.HasPrefix(authHeader, "http://") || strings.HasPrefix(authHeader, "https://") {
			dsn := strings.TrimPrefix(authHeader, "Bearer ")
			dsn = strings.TrimPrefix(dsn, "DSN ")
			query := fmt.Sprintf("SELECT org_id, project_id FROM %s WHERE dsn = '%s' LIMIT 1",
				hubTable("projects"), escapeSQL(dsn))
			rows, err := queryClient.Query(query)
			if err == nil && len(rows) > 0 {
				ctx.OrganizationID = getString(rows[0], "org_id")
				ctx.ProjectID = getString(rows[0], "project_id")
				return ctx, nil
			}
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) == 2 && (parts[0] == "Bearer" || parts[0] == "ApiKey") {
			keyParts := strings.Split(parts[1], ":")
			if len(keyParts) >= 2 {
				keyHash := parts[1]
				if len(keyParts) >= 3 {
					keyHash = strings.Join(keyParts[2:], ":")
				}
				query := fmt.Sprintf("SELECT org_id, project_id FROM %s WHERE key_hash = '%s' LIMIT 1",
					hubTable("api_keys"), escapeSQL(keyHash))
				rows, err := queryClient.Query(query)
				if err == nil && len(rows) > 0 {
					ctx.OrganizationID = getString(rows[0], "org_id")
					ctx.ProjectID = getString(rows[0], "project_id")
					return ctx, nil
				}
				return &TenantContext{}, fmt.Errorf("unauthorized: invalid API key")
			}
		}
	}

	parsed := opentenant.FromRequest(r)
	*ctx = parsed
	return ctx, nil
}

func tenantScopeSQL(r *http.Request, q *ClickHouseQuery, prefix string) string {
	ctx, _ := ExtractTenantContext(r, q)
	return ctx.ScopeAnd(prefix)
}

// tenantScopeSQLWithOwner scopes like tenantScopeSQL and, for organization
// requests, excludes personal rows (user_id set). Use only on tables that carry
// user_id (load_scenarios); other tables must keep tenantScopeSQL.
func tenantScopeSQLWithOwner(r *http.Request, q *ClickHouseQuery, prefix string) string {
	ctx, _ := ExtractTenantContext(r, q)
	scope := ctx.ScopeAnd(prefix)
	if ctx.OrgScoped() && !ctx.PersonalScoped() {
		scope += " AND " + opentenant.ExcludePersonalRows(prefix)
	}
	return scope
}

func AddTenantContext(r *http.Request, ctx *TenantContext) {
	if ctx == nil {
		return
	}
	ctx.Apply(r)
}

const tenantOAMRequiredMsg = "PEER_OAM_URL required when auth enabled"

func TenantMiddleware(handler http.HandlerFunc, queryClient *ClickHouseQuery) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, err := ExtractTenantContext(r, queryClient)
		if err != nil {
			http.Error(w, "invalid tenant context", 400)
			return
		}

		// When OAM owns the directory, tenant headers come from JWT-bound auth
		// middleware — skip opa.* existence checks that predate OAM wiring.
		if oamDirectoryConfigured() {
			AddTenantContext(r, ctx)
			handler(w, r)
			return
		}
		// Auth-on without OAM must not fall back to hub/opa directory tables.
		if authEnforced {
			http.Error(w, tenantOAMRequiredMsg, http.StatusServiceUnavailable)
			return
		}

		query := fmt.Sprintf("SELECT org_id FROM %s WHERE org_id = '%s' LIMIT 1",
			hubTable("organizations"), escapeSQL(ctx.OrganizationID))
		rows, err := queryClient.Query(query)
		if err != nil || len(rows) == 0 {
			http.Error(w, "organization not found", 404)
			return
		}

		query = fmt.Sprintf("SELECT project_id FROM %s WHERE org_id = '%s' AND project_id = '%s' LIMIT 1",
			hubTable("projects"), escapeSQL(ctx.OrganizationID), escapeSQL(ctx.ProjectID))
		rows, err = queryClient.Query(query)
		if err != nil || len(rows) == 0 {
			http.Error(w, "project not found", 404)
			return
		}

		AddTenantContext(r, ctx)
		handler(w, r)
	}
}

func GenerateDSN(orgID, projectID string) string {
	hash := base64.URLEncoding.EncodeToString([]byte(orgID + ":" + projectID))
	return fmt.Sprintf("http://%s@agent:8080/%s/%s", hash, orgID, projectID)
}

func AddTenantFilter(query string, ctx *TenantContext) string {
	if ctx == nil {
		return query
	}
	return opentenant.AddFilter(query, *ctx)
}
