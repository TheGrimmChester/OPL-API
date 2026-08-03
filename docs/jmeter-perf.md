# JMeter-compatible Open Perf Lab

Visual scenario builder in the Dashboard generates Apache JMeter `.jmx`. Runs execute in **ephemeral Docker containers** (`justb4/jmeter` by default). Host `OPA_JMETER_BIN` and Node `load-runner.mjs` are **dev-only** opt-ins.

## Honesty

- JMeter-compatible designer; users do **not** need to know JMeter — Design tab builds steps and Agent stores `jmx_xml`.
- JMX import is best-effort for HTTP samplers, timers, extractors, CSV, classic thread groups.
- Federation fan-out ≠ multi-region load cloud.
- Not a full plugin marketplace / Arrivals ThreadGroup / Playwright hybrid VU product.
- Generated/simple scenarios enforce URL policy (no private/metadata/decimal hosts) before validate/dispatch.
- **Raw JMX** may still hit arbitrary hosts via `HTTPSamplerProxy` even when script/OS samplers are blocked — treat imported JMX as trusted admin input.
- SLA gate is fail-closed; do not trust client-posted run `status` alone — use `/gate`.

## Engine model

```
Dashboard / API  →  Agent dispatchJMeterRunScaled
                 →  PerfContainerRunner (DockerRunner)
                 →  N × docker run --rm justb4/jmeter … (shared volume)
                 →  parse JTL → load_runs / load_run_samples
```

`PerfContainerRunner` is the extension point for future Kubernetes / other container APIs (not implemented yet).

## Env

| Variable | Default | Meaning |
|----------|---------|---------|
| `OPA_PERF_ENGINE` | `jmeter` | `jmeter` or `node` (node requires allow flag) |
| `OPA_JMETER_IMAGE` | `justb4/jmeter:5.5` | JMeter container image |
| `OPA_JMETER_WORK` | `$TMPDIR/opa-jmeter` | Agent-local work root for JMX/JTL |
| `OPA_JMETER_VOLUME` | — | Named Docker volume for shared mounts (compose) |
| `OPA_JMETER_HOST_WORK` | — | Host bind path equivalent of `OPA_JMETER_WORK` |
| `OPA_JMETER_NETWORK` | — | Docker network for worker containers |
| `OPA_JMETER_CPUS` | — | `--cpus` limit per worker |
| `OPA_JMETER_MEMORY` | — | `--memory` limit per worker |
| `OPA_JMETER_WORKERS` | `1` | Default container count (VU scale) |
| `OPA_JMETER_MAX_WORKERS` | `16` | Cap on workers |
| `OPA_PERF_ALLOW_HOST_JMETER` | off | `1` to allow host `OPA_JMETER_BIN` / PATH jmeter |
| `OPA_JMETER_BIN` | `jmeter` | Host executable (**dev-only**, needs allow flag) |
| `OPA_PERF_ALLOW_NODE_FALLBACK` | off | `1` to allow Node `load-runner.mjs` |
| `OPA_PERF_AUTO_DISPATCH` | off | `1` to always spawn engine on run create |
| `OPA_PERF_RUNNER` | — | `exec` enables Node spawn when allow flag is set |
| `OPA_LOAD_RUNNER_PATH` | `scripts/load-runner.mjs` | Node runner path (dev-only) |
| `OPA_PERF_RUNNER_TOKEN` | — | Shared secret for `POST .../metrics` |
| `OPA_PERF_MAX_VUS` | `100` | Cap virtual users |
| `OPA_PERF_MAX_DURATION` | `600` | Cap duration seconds |
| `OPA_PERF_ALLOW_UNSAFE_JMX` | off | Allow BeanShell/JSR223/BSF/OS samplers |
| `OPA_PERF_ALLOW_EMPTY_SLA` | off | `1` to pass gate when both sla and thresholds are empty |
| `OPA_FEDERATION_TOKEN` | — | Required for federation peer endpoints when auth on |

## APIs

- `POST /api/perf/scenarios/upsert` — steps, datasets, sla, schedule, optional `jmx_xml` (auto-generated if omitted). HTTP steps may include `selector_type` (`css`|`xpath`|`correlate`), `selector`, `page_url`, `ui_action` (correlation metadata; mirrored as JMX comments).
- `POST /api/perf/scenarios/import-jmx` — raw XML or `{name,jmx}`
- `POST /api/perf/scenarios/import-har` — HAR JSON (`log.entries`) or `{name,har,dry_run,include_static,id}`; maps to HTTP samplers
- `POST /api/perf/scenarios/import-xhr` — XHR JSON array / `{name,xhr,…}`; optional per-row selectors
- `GET /api/perf/scenarios/{id}` / `export-jmx` / `export-xhr` / `export-har` / `POST .../validate` / `.../gate`
- `POST /api/perf/runs` — `{scenario_id, dispatch, engine, fanout, profile, workers}`
- `POST /api/perf/runs/{id}/metrics` — admin or runner token; server recomputes pass/fail via SLA
- `GET /api/perf/runs/{id}/samples?since=` · `.../gate`

## Scale

`workers` (or `OPA_JMETER_WORKERS`) splits VUs across N ephemeral containers sharing the same `load_run_id`. Federation `fanout` still means peer agents, not a global load cloud.

## CLI

```bash
./scripts/jmeter-run.sh scripts/fixtures/sample-http.jmx /tmp/out.jtl run-demo
# Dev-only Node:
OPA_PERF_ALLOW_NODE_FALLBACK=1 OPA_PERF_RUNNER=exec node scripts/load-runner.mjs --scenario /tmp/scn.json --agent http://127.0.0.1:8080 --profile ramp
# Harness (OPA-stack): AGENT_TOKEN=... ./harness/jmeter-perf-gate.sh
```

## Out of scope

Full JMeter plugin fidelity, multi-cloud public generators, Playwright hybrid VUs, auto-fix PRs, JVM/.NET agents, Kubernetes Job runner (interface reserved).

## Security notes

- Scenario upsert / import-jmx / import-har / import-xhr / validate / dispatch / fan-out require **admin** when `OPA_AUTH_REQUIRED=1`.
- Metrics POST requires **admin** or `OPA_PERF_RUNNER_TOKEN` (viewers cannot forge pass/fail).
- Viewers may list scenarios and create undispatched run IDs for correlation; export-jmx/xhr/har are view-scoped.
- Validate uses dial-pinned HTTP (DNS rebinding resistant) and treats only **2xx** as OK.
- HAR/XHR import skips private/metadata hosts via the same URL policy as validate/dispatch.
- Scenario gate rejects runs whose `scenario_id` does not match the URL.
- SLA gate is fail-closed (empty/running summaries and empty SLA fail unless `OPA_PERF_ALLOW_EMPTY_SLA=1`).
