# Interop

| Variable | Purpose |
|----------|---------|
| `PEER_OPA_URL` | OPA hub for optional `load_run_id` correlation; selects co-deployed auth when `AUTH_MODE` unset |
| `PEER_ORA_URL` | Optional ORA base URL |
| `PEER_OSA_URL` | Optional OSA base URL |
| `OPEN_SERVICE_JWT_SECRET` | Service JWT mint/validate |
| `JWT_SECRET` | User JWT secret |
| `AUTH_MODE` | `standalone` (local `/api/auth/login`) or `codeployed` (hub-issued tokens) |
| `CLICKHOUSE_DB` | ClickHouse database for this product (default `opl`) |
| `OPL_PUBLIC_URL` | Public URL for this product |

## User auth modes

User JWTs and standalone `/api/auth/*` come from **Open-Auth-Go** (`Gate`); this repo keeps thin wiring in `auth_wire.go`.

| Mode | Behavior |
|------|----------|
| **Standalone** | `opl-api` issues JWTs locally. Lab admin: `admin`/`admin`. |
| **Co-deployed** | Share `JWT_SECRET` with **OPA-Hub**; hub issues; `opl-api` validates. |

Scopes typically used: `ingest:load_run`, `traces:read`, `health:read`.

OPL writes product tables into `CLICKHOUSE_DB` (default `opl`). Hub org/project/API-key lookups use database `opa`.

## Tenant headers

When `OPA_AUTH_REQUIRED=1`, send **`X-Organization-ID`** and **`X-Project-ID`** on Perf Lab list/create routes. Omitting them (or sending the picker marker `"all"`) scopes to **`default-org` / `default-project`** — the same write tenant used for INSERT — so lists match rows created without headers. Use a concrete org/project (e.g. `nas` / `infra`) to see that tenant's data.

```bash
TOKEN=$(curl -sf -X POST http://127.0.0.1:18080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | jq -r .token)

# Default write tenant (no headers, or after "all" is stripped)
curl -sf http://127.0.0.1:8092/api/perf/scenarios \
  -H "Authorization: Bearer $TOKEN" | jq '.scenarios | length'

curl -sf http://127.0.0.1:8092/api/perf/scenarios \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Organization-ID: default-org" \
  -H "X-Project-ID: default-project" | jq '.scenarios | length'

curl -sf "http://127.0.0.1:8092/api/perf/runs?limit=5" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Organization-ID: nas" \
  -H "X-Project-ID: infra" | jq '.runs | length'
```

From the LAN use `192.168.100.101` instead of `127.0.0.1`. Family overview: [OPA-Stack interop](https://github.com/TheGrimmChester/OPA-Stack/blob/main/docs/interop.md#tenant-headers-required-when-auth-is-on).
