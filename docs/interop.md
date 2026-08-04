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

When `OPA_AUTH_REQUIRED=1`, send **`X-Organization-ID`** and **`X-Project-ID`** on Perf Lab list/create routes. Without them, `GET /api/perf/scenarios` and `GET /api/perf/runs` return **HTTP 200 with empty arrays** even when `opl.load_scenarios` / `opl.load_runs` have rows.

```bash
TOKEN=$(curl -sf -X POST http://127.0.0.1:18080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | jq -r .token)

curl -sf http://127.0.0.1:8092/api/perf/scenarios \
  -H "Authorization: Bearer $TOKEN" | jq '.scenarios | length'   # → 0

curl -sf http://127.0.0.1:8092/api/perf/scenarios \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Organization-ID: default-org" \
  -H "X-Project-ID: default-project" | jq '.scenarios | length'   # → >0 when seeded

curl -sf "http://127.0.0.1:8092/api/perf/runs?limit=5" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Organization-ID: default-org" \
  -H "X-Project-ID: default-project" | jq '.runs | length'
```

From the LAN use `192.168.100.101` instead of `127.0.0.1`. Family overview: [OPA-Stack interop](https://github.com/TheGrimmChester/OPA-Stack/blob/main/docs/interop.md#tenant-headers-required-when-auth-is-on).
