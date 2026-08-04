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
| `OPL_RUN_WEBHOOK_URL` | Optional HTTPS/HTTP webhook for terminal run status (`completed` / `passed` / `failed` / `cancelled`, …). Empty disables notify. |
| `OPL_RUN_NOTIFY_MODE` | `deliver` (default) posts to the webhook; `log` / `log-only` / `dry-run` records intent only (no outbound HTTP) |
| `OPL_RUN_NOTIFY_STATUSES` | Comma list to filter (`failed,cancelled`) or `terminal` / `*` / empty for all terminal statuses |
| `OPL_RUN_WEBHOOK_SECRET` | Optional HMAC-SHA256 secret; sent as `X-OPL-Signature: sha256=<hex>` over the JSON body |

Product load tables (`load_scenarios`, `load_runs`, `load_run_samples`) are qualified with `CLICKHOUSE_DB` (default `opl`). Shared tenant directory tables (`organizations`, `projects`, `api_keys`, `federation_peers`) stay in the hub database `opa`.

### Terminal-run webhook

When a run reaches a terminal status (dispatch failure, engine finish, metrics POST, cancel, JTL import, scheduler dispatch fail), `opl-api` POSTs JSON:

```json
{
  "event": "opl.run.terminal",
  "service": "opl-api",
  "run_id": "…",
  "scenario_id": "…",
  "organization_id": "…",
  "project_id": "…",
  "status": "failed",
  "vus": 10,
  "error": "…",
  "source": "jmeter",
  "timestamp": "2026-08-04T12:00:00Z",
  "summary": {},
  "run_url": "https://…/?tab=results&run=…"
}
```

`GET /api/health` includes `run_notify` (`configured`, `mode`, `statuses`, optional `url_host` / `signed`) without exposing the full webhook URL or secret. Keep secrets in compose `.env`, not git.