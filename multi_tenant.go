package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel values shared by the dashboard's org/project pickers and the storage
// layer. "all" is the picker's only marker — the explicit "do not filter this
// dimension" value. The default-* ids are an ordinary org/project (the tenant
// that rows with empty ids normalize to), never a "nothing selected" state.
const (
	tenantAll        = "all"
	defaultOrgID     = "default-org"
	defaultProjectID = "default-project"
)

type TenantContext struct {
	OrganizationID string
	ProjectID      string
}

// Extract tenant context from request
// Supports:
// 1. DSN-based authentication: Authorization header with DSN
// 2. API key: Authorization header with API key format {org_id}:{project_id}:{key_hash}
// 3. Headers: X-Organization-ID and X-Project-ID
// 4. Query parameters: organization_id and project_id
func ExtractTenantContext(r *http.Request, queryClient *ClickHouseQuery) (*TenantContext, error) {
	ctx := &TenantContext{
		OrganizationID: "",
		ProjectID:      "",
	}

	// Try DSN-based authentication first
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		// Check if it's a DSN (starts with http:// or https://)
		if strings.HasPrefix(authHeader, "http://") || strings.HasPrefix(authHeader, "https://") {
			// Extract org/project from DSN by querying projects table
			dsn := strings.TrimPrefix(authHeader, "Bearer ")
			dsn = strings.TrimPrefix(dsn, "DSN ")
			query := fmt.Sprintf("SELECT org_id, project_id FROM opa.projects WHERE dsn = '%s' LIMIT 1",
				escapeSQL(dsn))
			rows, err := queryClient.Query(query)
			if err == nil && len(rows) > 0 {
				ctx.OrganizationID = getString(rows[0], "org_id")
				ctx.ProjectID = getString(rows[0], "project_id")
				return ctx, nil
			}
		}

		// Try API key format: {org_id}:{project_id}:{key_hash}
		parts := strings.Split(authHeader, " ")
		if len(parts) == 2 && (parts[0] == "Bearer" || parts[0] == "ApiKey") {
			keyParts := strings.Split(parts[1], ":")
			if len(keyParts) >= 2 {
				// Query API keys table to verify and get org/project.
				// org/project are derived ONLY from the stored record;
				// the client-supplied segments are never trusted.
				keyHash := parts[1]
				if len(keyParts) >= 3 {
					keyHash = strings.Join(keyParts[2:], ":")
				}
				query := fmt.Sprintf("SELECT org_id, project_id FROM opa.api_keys WHERE key_hash = '%s' LIMIT 1",
					escapeSQL(keyHash))
				rows, err := queryClient.Query(query)
				if err == nil && len(rows) > 0 {
					ctx.OrganizationID = getString(rows[0], "org_id")
					ctx.ProjectID = getString(rows[0], "project_id")
					return ctx, nil
				}
				// API key not found (or lookup failed): reject. Do NOT fall
				// back to client-supplied org/project — trusting keyParts[0]/[1]
				// would let anyone read any tenant's data by forging the
				// Authorization value.
				return &TenantContext{}, fmt.Errorf("unauthorized: invalid API key")
			}
		}
	}

	// Try headers.
	// "all" is KEPT here (auth off) so the two dimensions can be scoped
	// independently: org=X + project=all must filter on the org alone. Dropping
	// it left the field empty, which the scoped predicate then compared against
	// '' — a filter that matches nothing, so every "All" page came back blank.
	//
	// SECURITY: when auth is ENFORCED a client may NOT use "all" to widen its
	// own scope, so the marker is stripped there. The field stays empty and is
	// treated as no-access (see IsAllTenants), never as cross-tenant reads.
	if orgID := r.Header.Get("X-Organization-ID"); orgID != "" && !(authEnforced && orgID == tenantAll) {
		ctx.OrganizationID = orgID
	}
	if projID := r.Header.Get("X-Project-ID"); projID != "" && !(authEnforced && projID == tenantAll) {
		ctx.ProjectID = projID
	}

	// Try query parameters (only if the headers didn't set it).
	if ctx.OrganizationID == "" {
		if orgID := r.URL.Query().Get("organization_id"); orgID != "" && !(authEnforced && orgID == tenantAll) {
			ctx.OrganizationID = orgID
		}
	}
	if ctx.ProjectID == "" {
		if projID := r.URL.Query().Get("project_id"); projID != "" && !(authEnforced && projID == tenantAll) {
			ctx.ProjectID = projID
		}
	}

	return ctx, nil
}

// authEnforced mirrors OPA_AUTH_REQUIRED (set from main). It gates tenant
// isolation strictness so the two are always consistent: enabling auth also
// enables strict multi-tenant scoping, and vice-versa.
var authEnforced bool

// orgScoped / projectScoped report whether that ONE dimension narrows the query.
// They are deliberately independent: the pickers are two filters, not a single
// composite key, so "org=acme + project=All" scopes on the org and leaves
// the project wide open (and vice-versa).
//
// "all" is the picker's explicit unscoped marker and an absent value means the
// caller never picked (e.g. a bare curl), so both read as "do not filter". The
// default-* ids are NOT markers: they name a real organization and project, and
// selecting them scopes to them like any other tenant.
//
// When auth is ENABLED both are always scoped: a missing/empty tenant must be
// treated as no-access (a filter that matches nothing), never as "all tenants" —
// otherwise a client could read across tenants by omitting the org/project.
//
// TODO: when auth is enabled, gate a genuine admin-wide view behind a verified
// admin role on the authenticated principal, not a client-supplied value.
func (ctx *TenantContext) orgScoped() bool {
	if authEnforced {
		return true
	}
	return ctx.OrganizationID != "" && ctx.OrganizationID != tenantAll
}

func (ctx *TenantContext) projectScoped() bool {
	if authEnforced {
		return true
	}
	return ctx.ProjectID != "" && ctx.ProjectID != tenantAll
}

// IsAllTenants reports whether the request should bypass tenant scoping
// entirely — true only when NEITHER dimension is scoped.
//
// When auth is DISABLED (the default personal/experimental posture) the agent is
// effectively single-tenant, so an untouched picker shows all data and the
// dashboard works without juggling org/project headers.
func (ctx *TenantContext) IsAllTenants() bool {
	return !ctx.orgScoped() && !ctx.projectScoped()
}

// scopeEq builds the coalesce-normalized equality used for every tenant filter:
// rows written before multi-tenancy (empty ids) count as the default tenant.
func scopeEq(prefix, column, fallback, value string) string {
	return fmt.Sprintf("coalesce(nullif(%s%s, ''), '%s') = '%s'", prefix, column, fallback, escapeSQL(value))
}

// ScopePredicate returns a parenthesized SQL boolean scoping a query to the
// dimensions this request actually selected, or "" when neither is (the
// all-tenants view). `prefix` qualifies the columns for aliased/joined queries
// (e.g. "child." → child.organization_id); pass "" for unqualified.
func (ctx *TenantContext) ScopePredicate(prefix string) string {
	parts := make([]string, 0, 2)
	if ctx.orgScoped() {
		parts = append(parts, scopeEq(prefix, "organization_id", defaultOrgID, ctx.OrganizationID))
	}
	if ctx.projectScoped() {
		parts = append(parts, scopeEq(prefix, "project_id", defaultProjectID, ctx.ProjectID))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// ScopeBool is ScopePredicate as an always-valid SQL boolean ("1=1" when
// unscoped), for embedding straight into a WHERE clause.
func (ctx *TenantContext) ScopeBool(prefix string) string {
	if p := ctx.ScopePredicate(prefix); p != "" {
		return p
	}
	return "1=1"
}

// ScopeAnd is ScopePredicate as an appendable conjunct (" AND (...)"), or "".
func (ctx *TenantContext) ScopeAnd(prefix string) string {
	if p := ctx.ScopePredicate(prefix); p != "" {
		return " AND " + p
	}
	return ""
}

// WriteTenant returns the concrete (org, project) that owns rows this request
// creates. "all" and unset collapse to the default tenant so a picker marker can
// never be persisted as an id.
func (ctx *TenantContext) WriteTenant() (string, string) {
	org, proj := ctx.OrganizationID, ctx.ProjectID
	if org == "" || org == tenantAll {
		org = defaultOrgID
	}
	if proj == "" || proj == tenantAll {
		proj = defaultProjectID
	}
	return org, proj
}

// OwnedRowPredicate matches exactly the rows this request's write tenant owns,
// including legacy rows stored with empty ids. Used by settings-style
// read-back/update/delete paths that must address one tenant's row rather than
// filter a listing.
func (ctx *TenantContext) OwnedRowPredicate(prefix string) string {
	org, proj := ctx.WriteTenant()
	return "(" + scopeEq(prefix, "organization_id", defaultOrgID, org) +
		" AND " + scopeEq(prefix, "project_id", defaultProjectID, proj) + ")"
}

// tenantScopeSQL returns a WHERE fragment (prefixed with " AND ") that scopes a
// spans_min-style query to the request's selected org/project, or "" when the
// request should see all tenants (default view / auth off). `prefix` qualifies
// the columns for aliased/joined queries (e.g. "ei." → ei.organization_id);
// pass "" for unqualified.
func tenantScopeSQL(r *http.Request, q *ClickHouseQuery, prefix string) string {
	ctx, _ := ExtractTenantContext(r, q)
	return ctx.ScopeAnd(prefix)
}

// Add tenant context to request
func AddTenantContext(r *http.Request, ctx *TenantContext) {
	r.Header.Set("X-Organization-ID", ctx.OrganizationID)
	r.Header.Set("X-Project-ID", ctx.ProjectID)
}

// Middleware to extract and validate tenant context
func TenantMiddleware(handler http.HandlerFunc, queryClient *ClickHouseQuery) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, err := ExtractTenantContext(r, queryClient)
		if err != nil {
			http.Error(w, "invalid tenant context", 400)
			return
		}

		// Validate organization exists
		query := fmt.Sprintf("SELECT org_id FROM opa.organizations WHERE org_id = '%s' LIMIT 1",
			escapeSQL(ctx.OrganizationID))
		rows, err := queryClient.Query(query)
		if err != nil || len(rows) == 0 {
			http.Error(w, "organization not found", 404)
			return
		}

		// Validate project exists and belongs to organization
		query = fmt.Sprintf("SELECT project_id FROM opa.projects WHERE org_id = '%s' AND project_id = '%s' LIMIT 1",
			escapeSQL(ctx.OrganizationID), escapeSQL(ctx.ProjectID))
		rows, err = queryClient.Query(query)
		if err != nil || len(rows) == 0 {
			http.Error(w, "project not found", 404)
			return
		}

		// Add context to request
		AddTenantContext(r, ctx)
		handler(w, r)
	}
}

// Generate DSN for a project
func GenerateDSN(orgID, projectID string) string {
	// Generate a unique DSN
	// Format: http://{hash}@agent:8080/{org_id}/{project_id}
	hash := base64.URLEncoding.EncodeToString([]byte(orgID + ":" + projectID))
	return fmt.Sprintf("http://%s@agent:8080/%s/%s", hash, orgID, projectID)
}

// AddTenantFilter adds organization_id and project_id WHERE clauses to a query
func AddTenantFilter(query string, ctx *TenantContext) string {
	tenantPredicate := fmt.Sprintf("organization_id = '%s' AND project_id = '%s'",
		escapeSQL(ctx.OrganizationID), escapeSQL(ctx.ProjectID))

	// Locate an existing WHERE clause (case-insensitive).
	if idx := strings.Index(strings.ToUpper(query), "WHERE"); idx >= 0 {
		// SECURITY: wrap the caller's existing condition in parentheses before
		// ANDing the tenant predicate. Without the parentheses an OR in the
		// caller's filter (e.g. "WHERE a=1 OR b=2") would bind as
		// "a=1 OR (b=2 AND org=.. AND project=..)", leaking cross-tenant rows.
		whereEnd := idx + len("WHERE")
		return query[:whereEnd] + " (" + strings.TrimSpace(query[whereEnd:]) + ") AND " + tenantPredicate
	}
	return query + " WHERE " + tenantPredicate
}
