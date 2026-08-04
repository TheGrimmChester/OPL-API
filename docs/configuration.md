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
| `OPL_RUN_NOTIFY_MODE` | `deliver` (default) contacts every configured channel; `log` / `log-only` / `dry-run` records intent only (no outbound HTTP, no mail) |
| `OPL_RUN_NOTIFY_STATUSES` | Comma list to filter (`failed,cancelled`) or `terminal` / `*` / empty for all terminal statuses |
| `OPL_RUN_NOTIFY_CHANNELS` | Comma list restricting channels (`webhook,chat,email`); empty / `*` enables all |
| `OPL_RUN_WEBHOOK_URL` | **webhook channel** — HTTPS/HTTP endpoint for the raw JSON event. Empty leaves the channel unconfigured. |
| `OPL_RUN_WEBHOOK_SECRET` | Optional HMAC-SHA256 secret for the webhook channel; sent as `X-OPL-Signature: sha256=<hex>` over the JSON body |
| `OPL_RUN_CHAT_WEBHOOK_URL` | **chat channel** — incoming-webhook URL; receives a message payload (`text` + colored attachment). Empty leaves the channel unconfigured. |
| `OPL_RUN_EMAIL_TO` | **email channel** — comma list of recipients. Empty leaves the channel unconfigured. |
| `OPL_RUN_EMAIL_FROM` | Optional From override (defaults to `OPA_SMTP_FROM`, then `OPA_SMTP_USER`) |
| `OPA_SMTP_HOST` / `OPA_SMTP_PORT` / `OPA_SMTP_USER` / `OPA_SMTP_PASS` / `OPA_SMTP_FROM` | SMTP relay for the email channel, shared with the edge agent alert email. Without `OPA_SMTP_HOST` the recipients are recorded as `logged` (intentional no-send), never dropped. |

### Scheduler and orchestrator

| Variable | Description |
|----------|-------------|
| `ORCHESTRATOR_LISTEN_ADDR` | `opl-orchestrator` listen address (default `:8097`) |
| `OPA_PERF_SCHEDULER_DISABLE` | `1` disables the schedule tick in whichever process reads it (both `opl-api` and `opl-orchestrator` honour it) |
| `OPA_PERF_SCHEDULER_TICK_SEC` | `opl-api` tick interval (default 60, floor 15) |
| `OPL_ORCHESTRATOR_TICK_SEC` | `opl-orchestrator` dispatch tick interval (default 30, floor 5) |
| `OPL_ORCHESTRATOR_DISPATCH_DISABLE` | `1` leaves the orchestrator serving state endpoints without firing schedules |
| `OPL_ORCHESTRATOR_REAP_DISABLE` | `1` disables the run reaper; runs then stay `running` until something else closes them |
| `OPL_ORCHESTRATOR_REAP_TICK_SEC` | Reaper tick interval (default 60, floor 5) |
| `OPL_SCHEDULER_OWNER` | Pins this process's lease identity (default `<role>/<host>/<pid>-<rand>`). Two processes must never share one value |
| `OPL_SCHEDULER_LEASE_SEC` | Lease TTL (default 300). Must exceed the tick interval, and bounds how long a dead owner blocks an occurrence |
| `OPL_SCHEDULER_LEASE_SETTLE_MS` | Arbitration settle window (default 2000). **`0` disables the single-fire guarantee under contention** — only safe with exactly one scheduler |
| `OPL_RUN_REAP_GRACE_SEC` | Minimum age before a `running` run is reap-eligible (default 120) |
| `OPL_RUN_MAX_SEC` | Hard deadline after which a `running` run is closed as `error` (default 7200; `0` disables deadline reaping) |

Product load tables (`load_scenarios`, `load_runs`, `load_run_samples`, `report_templates`, `run_notifications`, `load_schedule_state`, `load_schedule_leases`, `load_schedule_fires`) are qualified with `CLICKHOUSE_DB` (default `opl`). Shared tenant directory tables (`organizations`, `projects`, `api_keys`, `federation_peers`) stay in the hub database `opa`.

### Terminal-run notifications

When a run reaches a terminal status (dispatch failure, engine finish, metrics POST, cancel, JTL import, scheduler dispatch fail), `opl-api` builds one event and offers it to every enabled channel. The webhook channel POSTs it verbatim:

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

The chat channel renders the same facts as a message payload (`text` plus a colored attachment); the email channel renders them as plain-text mail.

`GET /api/health` includes `run_notify` with `configured`, `mode`, `statuses`, `channels_ready` and a `channels[]` array. Each entry carries `name`, `enabled`, `configured`, a redacted `target` (scheme+host only, or a recipient count) and, when a channel is not usable, a plain `reason`. Full URLs, tokens, SMTP passwords and recipient addresses are never exposed. Keep every secret in compose `.env`, not git.

Every attempt is persisted to `<CLICKHOUSE_DB>.run_notifications` and served by `GET /api/perf/notifications` (and `GET /api/perf/runs/{id}/notifications`) with result `sent` / `failed` / `logged` / `skipped`; a `skipped` row names the missing setting in `detail`. `POST /api/perf/notifications/test` (admin) fires a synthetic event through every channel.

### Report and trend templates

Saved layouts live in `<CLICKHOUSE_DB>.report_templates`, scoped by `X-Organization-ID` / `X-Project-ID` like every other lab object. There is no environment variable to configure — see [perf-lab.md](perf-lab.md#report-and-trend-templates).