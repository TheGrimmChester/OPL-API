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
| `POST .../scenarios/{id}/validate` | 1 VU dry-run; `ok`/`pass` + `triage[]` + `correlation_suggestions[]` |
| `POST /api/perf/scenarios/import-har` | HAR → HTTP steps (+ optional upsert); `dry_run=1` previews |
| `POST /api/perf/scenarios/import-xhr` | XHR JSON → HTTP steps with optional selectors |
| `POST /api/perf/scenarios/import-postman` | Postman Collection v2/v2.1 → HTTP steps |
| `GET /api/perf/load-policies` | Presets: smooth→ramp, sustained→soak, stress→spike, custom (+ `curve` points) |
| `POST /api/perf/runs` | Start run; optional `fanout`, `profile`/`policy`, `workers`, `dispatch`. Status is `running` only when an engine is dispatched; `created` when `dispatch:false`; `failed` when dispatch errors. |
| `POST /api/perf/runs/import-jtl` | Admin — import JMeter JTL → `load_runs` + samples |
| `POST /api/perf/runs/{id}/cancel` | Admin — mark cancelled + best-effort `docker stop` on registered workers |
| `GET /api/perf/runs/{id}/runners` | Live `docker inspect` status for dispatched containers |
| `GET /api/perf/runs/{id}/steps` | Per-label aggregates (avg/p95/error_rate) |
| `GET /api/perf/runs/{id}/report` | Bench report JSON; `?format=csv` for CSV |
| `POST /api/perf/runs/{id}/metrics` | Runner posts summary + samples |
| `GET /api/perf/runs/{id}/export-k6` | k6 script export |
| `GET /api/perf/runs/{id}/gate` | SLA gate (`ok` + `pass` booleans) |
| `GET /api/health` | Includes `run_notify` (webhook configured / mode; no secrets) |
| `POST /api/federation/remote-load` | **Agent** — peer-local load sample (not served here) |
| `GET /api/performance/baselines` + `/api/performance/gate` | **Agent** — Profiling baselines / gate |

### Terminal-run notifications

Set `OPL_RUN_WEBHOOK_URL` (compose `.env`) to POST JSON when a run becomes terminal. Filter with `OPL_RUN_NOTIFY_STATUSES` (default all terminal). Use `OPL_RUN_NOTIFY_MODE=log` for safe E2E without outbound HTTP. See [configuration.md](configuration.md).

### Nested steps / visual editor backend

`steps_json` may nest `children` under `transaction`/`container`, `if`/`while`/`loop`/`foreach`, `fragment`, and `http` (extract/assert). `include`/`link` expands a named fragment at validate/JMX emit. JMX emission opens nested `hashTree`s (`IfController` / `WhileController` / `LoopController` / `ForeachController` / `TransactionController` / disabled `GenericController` for fragments); validate flattens depth-first via `flattenScenarioSteps`. Import prefers a nested tree parse so controllers round-trip.

### Custom load curve

`schedule_json.curve`: `[{ "t": 0, "vus": 0 }, { "t": 30, "vus": 20 }, …]`. On run/schedule, OPL maps peak VUs + duration + `ramp_seconds` onto classic ThreadGroup (honesty: not arrivals-accurate injectors).

### Light scheduler

`schedule_json`: `{ "enabled": true, "every_minutes": 60 }` or `{ "enabled": true, "daily_at": "02:30" }` (UTC). `startPerfScheduler` ticks in-process (disable with `OPA_PERF_SCHEDULER_DISABLE=1`).

### Tenant headers

With `OPA_AUTH_REQUIRED=1`, list routes (`GET /api/perf/scenarios`, `GET /api/perf/runs`) scope ClickHouse via `X-Organization-ID` / `X-Project-ID`. Omitting them (or sending `"all"`) scopes to `default-org` / `default-project`, matching writes. See [interop.md](interop.md#tenant-headers).

OPA ingest tags spans with `load_run_id` when it sees `X-OPA-Load-Run-Id` or baggage. OPL-Dashboard presets + baselines panel; Trace Explorer folds `?load_run_id=` into the filter DSL.

## CI

See OPA-stack `harness/perf-gate.sh`, `harness/jmeter-perf-gate.sh`, and `.github/workflows/perf-gate.yml.example`.
