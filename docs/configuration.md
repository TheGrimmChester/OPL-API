# Configuration

| Variable | Description |
|----------|-------------|
| `LISTEN_ADDR` / `HTTP_ADDR` | HTTP listen address (smoke default `:8092`) |
| `JWT_SECRET` | User JWT secret (issue in standalone; validate in co-deployed) |
| `AUTH_MODE` | `standalone` or `codeployed` (default: standalone when `PEER_OPA_URL` empty) |
| `AUTH_ADMIN_USER` / `AUTH_ADMIN_PASSWORD` | Lab admin seed for standalone login |
| `OPEN_SERVICE_JWT_SECRET` | Service JWT mint/validate secret |
| `CLICKHOUSE_URL` | ClickHouse HTTP endpoint |
| `CLICKHOUSE_DB` | Product database (default `opl`). Alias: `CLICKHOUSE_DATABASE` |
| `PEER_OPA_URL` | Optional OPA hub URL for load-run correlation |
| `OPL_PUBLIC_URL` | Public URL for this product |

Product load tables (`load_scenarios`, `load_runs`, `load_run_samples`) are qualified with `CLICKHOUSE_DB` (default `opl`). Shared tenant directory tables (`organizations`, `projects`, `api_keys`, `federation_peers`) stay in the hub database `opa`.
