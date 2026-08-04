# Changelog

## Unreleased

- Terminal-run notifications: optional webhook (`OPL_RUN_WEBHOOK_URL`) on `completed` / `passed` / `failed` / `cancelled` (and aliases); `OPL_RUN_NOTIFY_MODE=log` for dry-run; optional HMAC `OPL_RUN_WEBHOOK_SECRET`; health exposes `run_notify` without secrets.
- ForEach + Fragment/Link in VU tree → JMX (`ForeachController`, disabled `GenericController` fragments, include expand); validate skips fragment defs and expands links.
- Postman Collection import: `POST /api/perf/scenarios/import-postman` (v2/v2.1 → HTTP steps; `{{var}}` → `${var}`).
- Validate auto-correlation: `correlation_suggestions[]` (token/CSRF/Bearer heuristics) alongside triage cards.
- Restore archived scenarios: `POST .../unarchive`; list `?archived=1`.
- Visual editor depth: If / While / Loop controllers in steps → JMX (`IfController` / `WhileController` / `LoopController`) with nested hashTrees; JMX import preserves controller nesting on round-trip.
- Custom load curve: `schedule_json.curve` points (`t`/`vus`) resolve via load-policies custom path → peak VUs + duration + ramp for ThreadGroup (honesty: not arrivals-accurate).
- Lab ops: scenario soft-archive + duplicate; `GET /api/perf/load-policies`; run runners live status; per-step stats + report (`?format=csv`); `POST /api/perf/runs/import-jtl`; validate triage (`pass` + `triage[]`); light `schedule_json` scheduler; instrumentation honesty for public vs compose demo hosts.
- JMeter visual editor backend: nested `children` on steps → TransactionController / HTTPSamplerProxy nested hashTrees; `flattenScenarioSteps` for validate.
- Auth: adopt Open-Auth-Go per-user project ACLs (`project_ids` / `EnforceProjectACL` on Gate middleware). Restricted JWTs get **403** on non-member `X-Project-ID`; role `admin` stays unrestricted. No second membership store — hub-minted claims only.
- Run lifecycle: `POST /api/perf/runs` writes `created` when undispatched and `failed` when dispatch errors (no more stuck `running`); `POST /api/perf/runs/{id}/cancel`.
- SLA gate JSON includes `pass` alongside `ok`/`status`.
- Docs: tenant headers required for `GET /api/perf/scenarios` and `GET /api/perf/runs` when auth is on; NAS curl examples in interop/perf-lab.
- Auth via Open-Auth-Go `Gate` (delete local `auth.go` / `auth_local.go` duplicates).
- Qualify Perf Lab tables with `CLICKHOUSE_DB` via `chTable()`; hub tenant tables use `hubTable()` (`opa`).
- Create `load_*` schema in the product database on startup.

- Product branding: Open Perf Lab (`opl-api` / `OPL-API`).
