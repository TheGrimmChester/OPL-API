# Perf lab

OPL-API (`opl-api`) owns Perf Lab HTTP APIs (scenarios, runs, JMeter). OPA owns
`tags.load_run_id` correlation, performance baselines/gate, and
`POST /api/federation/remote-load` + the peer registry.

Load scenarios correlate into APM via `X-OPA-Load-Run-Id` / baggage `load_run_id`,
with optional **federation peer fan-out**. JMeter Perf Lab adds **Docker JMeter** as the
production execution engine.

## Honesty

- Production runs: ephemeral JMeter containers via `PerfContainerRunner` / `DockerRunner` (`OPA_JMETER_IMAGE`).
- Node `scripts/load-runner.mjs` and host `OPA_JMETER_BIN` are **dev-only** (`OPA_PERF_ALLOW_NODE_FALLBACK=1` / `OPA_PERF_ALLOW_HOST_JMETER=1`).
- `fanout: true` on `POST /api/perf/runs` loads peers from `OPA_FEDERATION_PEERS` and/or ClickHouse `opa.federation_peers`, then POSTs to each peer’s Agent `POST /api/federation/remote-load` and merges metrics.
- **Without configured peers, fan-out is local-sample-only** — responses say so; do not claim live peers.
- **Multi-peer fan-out ≠ multi-cloud commercial load grid** — better than one runner, still not a public load cloud.
- Container worker scale (`workers` / `OPA_JMETER_WORKERS`) splits VUs across N JMeter containers on the same Perf-Lab host.

## Profiles

`OPA_PERF_ALLOW_NODE_FALLBACK=1 node scripts/load-runner.mjs --scenario file.json --profile soak|spike|ramp`

| Profile | Defaults |
|---------|----------|
| `soak` | low VUs, long duration |
| `spike` | high VUs, short burst |
| `ramp` | VUs grow over first half of duration |

`POST /api/perf/runs` also accepts `"profile": "soak"|"spike"|"ramp"` and `"workers": N`.

## APIs

| Endpoint | Role |
|----------|------|
| `GET/POST /api/perf/scenarios` + upsert | Scenario CRUD (steps may include nested `children`, CSS/XPath selector metadata). List/get hide soft-archived rows. |
| `DELETE` / `POST .../scenarios/{id}/archive` | Soft-archive (`archived=1`) |
| `POST .../scenarios/{id}/unarchive` | Restore soft-archived scenario (`archived=0`) |
| `GET /api/perf/scenarios?archived=1` | List soft-archived scenarios |
| `POST .../scenarios/{id}/duplicate` | Clone scenario (optional `{name}`) |
| `POST .../scenarios/{id}/schedule` | Patch `schedule_json` (`enabled`, `every_minutes`, `daily_at`, optional `curve`) |
| `POST .../scenarios/{id}/validate` | 1 VU dry-run seeded with the first dataset row; `ok`/`pass` + `triage[]` + `correlation_suggestions[]` + `unbound_variables[]` + `dataset` |
| `POST /api/perf/scenarios/import-har` | HAR → HTTP steps (+ optional upsert); `dry_run=1` previews |
| `POST /api/perf/scenarios/import-xhr` | XHR JSON → HTTP steps with optional selectors |
| `POST /api/perf/scenarios/import-postman` | Postman Collection v2/v2.1 → HTTP steps |
| `GET /api/perf/load-policies` | Presets: smooth→ramp, sustained→soak, stress→spike, custom (+ `curve` points) |
| `POST /api/perf/runs` | Start run; optional `fanout`, `profile`/`policy`, `workers`, `dispatch`. Status is `running` only when an engine is dispatched; `created` when `dispatch:false`; `failed` when dispatch errors. |
| `POST /api/perf/runs/import-jtl` | Admin — import JMeter JTL → `load_runs` + samples |
| `POST /api/perf/runs/{id}/cancel` | Admin — mark cancelled + best-effort `docker stop` on registered workers |
| `GET /api/perf/runs/{id}/runners` | Live `docker inspect` status for dispatched containers |
| `GET /api/perf/runs/{id}/steps` | Per-label aggregates (avg/p95/error_rate) |
| `GET /api/perf/runs/{id}/report` | Bench report JSON; `?format=csv|html|pdf`; `?template=<id>` applies a saved layout |
| `GET /api/perf/runs/{id}/bench-pack` | ZIP bench pack (JSON + CSV + HTML + PDF + MANIFEST); `?template=<id>` |
| `GET /api/perf/scenarios/{id}/trends` | Multi-run trend series (`points`, best/worst p95, SLA breaches); `?limit=` `?sla_p95_ms=` `?template=<id>` |
| `GET /api/perf/report-templates` | List saved report/trend layouts (`?kind=report|trend`) |
| `POST /api/perf/report-templates/upsert` | Admin — create/update a layout (`name`, `kind`, `widgets`, `metrics`, `window`, `options`) |
| `GET /api/perf/report-templates/{id}` | Fetch one layout |
| `DELETE` / `POST .../report-templates/{id}/archive` | Admin — soft-archive a layout |
| `GET /api/perf/notifications` | Notification history across runs (`?limit=` `?channel=` `?result=` `?run_id=`) |
| `GET /api/perf/runs/{id}/notifications` | Per-channel delivery attempts for one run |
| `POST /api/perf/notifications/test` | Admin — fire a synthetic terminal event through every channel |
| `POST /api/perf/runs/{id}/metrics` | Runner posts summary + samples |
| `GET /api/perf/runs/{id}/export-k6` | k6 script export |
| `GET /api/perf/runs/{id}/gate` | SLA gate (`ok` + `pass` booleans) |
| `GET /api/health` | Includes `run_notify` (per-channel configured / mode / redacted target; no secrets) |
| `POST /api/federation/remote-load` | **Agent** — peer-local load sample (not served here) |
| `GET /api/performance/baselines` + `/api/performance/gate` | **Agent** — Profiling baselines / gate |

### Terminal-run notifications

Three channels share one terminal-run event and one set of controls:

| Channel | Destination | Configured by |
|---------|-------------|---------------|
| `webhook` | raw JSON POST, optionally HMAC-signed | `OPL_RUN_WEBHOOK_URL` (+ `OPL_RUN_WEBHOOK_SECRET`) |
| `chat` | chat incoming-webhook message payload (`text` + colored attachment) | `OPL_RUN_CHAT_WEBHOOK_URL` |
| `email` | plain-text SMTP mail | `OPL_RUN_EMAIL_TO` + `OPA_SMTP_HOST` (shared stack SMTP block) |

Filter statuses with `OPL_RUN_NOTIFY_STATUSES` (default all terminal), restrict channels with `OPL_RUN_NOTIFY_CHANNELS`, and use `OPL_RUN_NOTIFY_MODE=log` for safe E2E without outbound HTTP or mail. Keep every URL, secret and recipient list in the compose `.env` — never in git. See [configuration.md](configuration.md).

**A notification is never silently dropped.** Every terminal run writes one history row per channel:

| Result | Meaning |
|--------|---------|
| `sent` | the destination accepted the delivery |
| `failed` | the channel is configured but the destination errored |
| `logged` | `OPL_RUN_NOTIFY_MODE=log`, or email recipients without an SMTP relay (intentional no-send) |
| `skipped` | the channel is disabled or has no destination — the `detail` column names the missing setting |

`GET /api/perf/notifications` and `GET /api/perf/runs/{id}/notifications` serve that history; `GET /api/health` `run_notify.channels[]` reports the same configuration state with hosts only (no paths, tokens, passwords or recipient addresses). `POST /api/perf/notifications/test` (admin) fires a synthetic event with zeroed metrics to prove wiring without launching a load run.

### Report and trend templates

A template is a named, org+project-scoped layout stored in `opl.report_templates` (created by `ensurePerfLabSchema`; the hub database is never written):

| Field | Meaning |
|-------|---------|
| `kind` | `report` (per-run bench report) or `trend` (multi-run scenario series) |
| `widgets` | report: `kpis`, `summary`, `steps`, `errors`, `samples` · trend: `kpis`, `latency_band`, `error_bars`, `runs_table` |
| `metrics` | any of `p50_ms`, `p95_ms`, `p99_ms`, `avg_ms`, `error_rate`, `samples` |
| `window` | trend: `limit` / `runs` (1–100) and `sla_p95_ms` · report: `sample_cap` |
| `options` | free-form extras (currently `sample_cap`) |

Unknown widget/metric names are dropped on save, so an export never claims a widget the product cannot render. Pass `?template=<id>` to `runs/{id}/report`, `runs/{id}/bench-pack` or `scenarios/{id}/trends`; the response carries a `template` block and the `X-OPL-Template` header. A template id that is missing, archived or of the wrong kind falls back to the full layout **and says so** in `template_note`. A template only selects what is rendered — it never changes how a run was measured.

### Nested steps / visual editor backend

`steps_json` may nest `children` under `transaction`/`container`, `if`/`while`/`loop`/`foreach`, `fragment`, and `http` (extract/assert). `include`/`link` expands a named fragment at validate/JMX emit. JMX emission opens nested `hashTree`s (`IfController` / `WhileController` / `LoopController` / `ForeachController` / `TransactionController` / disabled `GenericController` for fragments); validate flattens depth-first via `flattenScenarioSteps`. Import prefers a nested tree parse so controllers round-trip.

### Parameterised data

`datasets_json.csv` (`variableNames`, `delimiter`, inline rows or a runner-local `filename`) is emitted
as a `CSVDataSet` at Test Plan level, so `${column}` binds at run time. The configured delimiter drives
both the `data.csv` written next to `plan.jmx` and the element in the plan. Validate lists every `${…}`
token that no column, extractor, ForEach variable, raw-JMX `Argument.name`, or plan built-in can bind in
`unbound_variables[]` and fails — a plan that would send literal `${…}` text never reports a clean pass.
Defaults (`recycle=true`, `stopThread=false`, `shareMode.all`, `quotedData=true`) and worker sharding are
documented in [jmeter-perf.md](jmeter-perf.md#parameterised-data-csv).

### Custom load curve

`schedule_json.curve` with `curve_mode`:

| Mode | Points | Injector |
|------|--------|----------|
| `vus` (default) | `[{ "t": 0, "vus": 0 }, { "t": 30, "vus": 20 }, …]` | Classic ThreadGroup peak VUs + duration + `ramp_seconds` (closed model) |
| `arrivals` | `[{ "t": 0, "rate": 0 }, { "t": 30, "rate": 2 }, …]` (`rate` = arrivals/sec) | Stock ThreadGroup **open-model segments**: trapezoid-integrated starts, `loops=1`, `ThreadGroup.delay` + ramp per segment |

Caps: `OPA_PERF_MAX_ARRIVALS` (default `OPA_PERF_MAX_VUS×20`, max 10000), `OPA_PERF_MAX_ARRIVAL_RATE` (default 100/s). Honesty strings on run/schedule responses distinguish concurrent-VU approximation from arrivals-accurate open model.

### Light scheduler

`schedule_json`: `{ "enabled": true, "every_minutes": 60 }` or `{ "enabled": true, "daily_at": "02:30" }` (UTC). `startPerfScheduler` ticks in-process (disable with `OPA_PERF_SCHEDULER_DISABLE=1`).

### Tenant headers

With `OPA_AUTH_REQUIRED=1`, list routes (`GET /api/perf/scenarios`, `GET /api/perf/runs`) scope ClickHouse via `X-Organization-ID` / `X-Project-ID`. Omitting them (or sending `"all"`) scopes to `default-org` / `default-project`, matching writes. See [interop.md](interop.md#tenant-headers).

OPA ingest tags spans with `load_run_id` when it sees `X-OPA-Load-Run-Id` or baggage. OPL-Dashboard presets + baselines panel; Trace Explorer folds `?load_run_id=` into the filter DSL.

## CI

See OPA-stack `harness/perf-gate.sh`, `harness/jmeter-perf-gate.sh`, and `.github/workflows/perf-gate.yml.example`.
