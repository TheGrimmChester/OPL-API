# Wave 29 — Perf lab (deepened)

Load scenarios correlated into APM via `X-OPA-Load-Run-Id` / baggage `load_run_id`, with optional **federation peer fan-out**. Wave 31 adds **Docker JMeter** as the production execution engine.

## Honesty

- Production runs: ephemeral JMeter containers via `PerfContainerRunner` / `DockerRunner` (`OPA_JMETER_IMAGE`).
- Node `scripts/load-runner.mjs` and host `OPA_JMETER_BIN` are **dev-only** (`OPA_PERF_ALLOW_NODE_FALLBACK=1` / `OPA_PERF_ALLOW_HOST_JMETER=1`).
- `fanout: true` on `POST /api/perf/runs` dispatches to federation peers via `POST /api/federation/remote-load` (peer runs concurrent HTTP locally or simulates) and merges metrics.
- **Multi-peer fan-out ≠ multi-cloud commercial load grid** — better than one runner, still not a public load cloud.
- Container worker scale (`workers` / `OPA_JMETER_WORKERS`) splits VUs across N JMeter containers on the same agent.

## Profiles

`OPA_PERF_ALLOW_NODE_FALLBACK=1 node scripts/load-runner.mjs --scenario file.json --profile soak|spike|ramp`

| Profile | Defaults |
|---------|----------|
| `soak` | low VUs, long duration |
| `spike` | high VUs, short burst |
| `ramp` | VUs grow over first half of duration |

Agent `POST /api/perf/runs` also accepts `"profile": "soak"|"spike"|"ramp"` and `"workers": N`.

## APIs

| Endpoint | Role |
|----------|------|
| `GET/POST /api/perf/scenarios` + upsert | Scenario CRUD (steps may include CSS/XPath selector metadata) |
| `POST /api/perf/scenarios/import-har` | HAR → HTTP steps (+ optional upsert); `dry_run=1` previews |
| `POST /api/perf/scenarios/import-xhr` | XHR JSON → HTTP steps with optional selectors |
| `POST /api/perf/runs` | Start run; optional `fanout`, `profile`, `workers`, `dispatch` |
| `POST /api/perf/runs/{id}/metrics` | Runner posts summary + samples |
| `GET /api/perf/runs/{id}/export-k6` | k6 script export |
| `POST /api/federation/remote-load` | Peer-local load sample |
| `GET /api/performance/baselines` + `/api/performance/gate` | Wave 11 baselines / gate |

Ingest tags spans with `load_run_id` when it sees `X-OPA-Load-Run-Id` or baggage. Dashboard Perf Lab presets + baselines panel; Trace Explorer folds `?load_run_id=` into the filter DSL.

## CI

See OPA-stack `harness/perf-gate.sh`, `harness/jmeter-perf-gate.sh`, and `.github/workflows/perf-gate.yml.example`.
