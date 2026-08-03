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

| Mode | Behavior |
|------|----------|
| **Standalone** | `opl-api` issues JWTs locally. Lab admin: `admin`/`admin`. |
| **Co-deployed** | Share `JWT_SECRET` with **OPA-Hub**; hub issues; `opl-api` validates. |

Scopes typically used: `ingest:load_run`, `traces:read`, `health:read`.

OPL writes product tables into `CLICKHOUSE_DB` (default `opl`). Hub org/project/API-key lookups use database `opa`.
