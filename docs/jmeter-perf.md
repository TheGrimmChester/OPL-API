# JMeter-compatible Open Perf Lab

Visual scenario builder in the Dashboard generates Apache JMeter `.jmx`. Runs execute in **ephemeral Docker containers** (`justb4/jmeter` by default). Host `OPA_JMETER_BIN` and Node `load-runner.mjs` are **dev-only** opt-ins.

## Honesty

- JMeter-compatible designer; users do **not** need to know JMeter — Design tab builds steps and Agent stores `jmx_xml`.
- JMX import is best-effort for HTTP samplers, timers, extractors, CSV, classic thread groups, and nested If/While/Loop/Transaction controllers when present.
- Scenario datasets emit a real `CSVDataSet`; a `filename` without inline rows stays **runner-local** — the file must exist where the engine runs.
- Unbound-variable detection covers plain `${name}` references only; anything computed inside a JMeter function is opaque and use-before-define ordering is not checked.
- Federation fan-out ≠ multi-region load cloud.
- Not a full plugin marketplace; no real-browser hybrid VU engine.
- Generated/simple scenarios enforce URL policy (no private/metadata/decimal hosts) before validate/dispatch.
- **HAR/XHR import** keeps lab RFC1918 / loopback / `host.docker.internal` in steps with warnings; cloud metadata stays blocked. Validate/dispatch dial-pin is **not** weakened — set `OPA_PERF_INTERNAL_HOSTS` for those lab hosts or they fail at run time.
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

- `POST /api/perf/scenarios/upsert` — steps (optional nested `children` including `if` / `while` / `loop` / `transaction`), `datasets` (bound into the emitted plan — see Honesty above), sla, schedule (`curve` points optional; classic ThreadGroup uses `schedule.ramp_seconds` when > 0, else **10**), optional `jmx_xml` (auto-generated if omitted). HTTP steps may include `headers` (map or `[{name,value}]` → per-sampler `HeaderManager`, separate from plan-level OPA correlation headers), `follow_redirects` (default true), `always_encode` (default false when body set), `connect_timeout_ms` / `response_timeout_ms` (emitted when > 0), `think_ms` / `think_ms_rand` (UniformRandomTimer when rand > think), and `enabled` (default true; `false` → JMX `enabled="false"`, skipped in validate). Extractors accept `match_number` / `template` / `default_value`; asserts map `assert_type` / `assert_field` / `assume_success`; transactions accept `include_timers` / `generate_parent_sample`; If accepts `evaluate_all` / `use_expression`; ForEach honors `use_separator`. HTTP steps may include `selector_type` (`css`|`xpath`|`correlate`), `selector`, `page_url`, `ui_action` (correlation metadata; mirrored as JMX comments). `fragment` steps are reusable definitions and `include` / `link` steps reference them by name with optional `params`; the response carries `fragment_references[]` saying per reference whether the plan emitted a module reference or fell back to an inline copy. A `rendezvous` step (or `schedule.rendezvous`) emits a synchronising timer — see [perf-lab.md](perf-lab.md#reusable-journey-modules).
- `POST /api/perf/scenarios/import-jmx` — raw XML or `{name,jmx}`; nested controller tree preserved when parseable
- `POST /api/perf/scenarios/import-har` — HAR JSON (`log.entries`) or `{name,har,dry_run,include_static,id}`; maps to HTTP samplers. **Lab RFC1918 / loopback / `host.docker.internal` are kept** with warnings (set `OPA_PERF_INTERNAL_HOSTS` before validate/dispatch); cloud metadata (`169.254.169.254`, `metadata.google.internal`, weird hosts) stay blocked. Empty imports return skip tallies (`static` / `private` / `OPTIONS` / `blocked` / `empty`).
- `POST /api/perf/scenarios/import-xhr` — XHR JSON array / `{name,xhr,…}`; optional per-row selectors
- `GET /api/perf/scenarios/{id}` / `export-jmx` / `export-xhr` / `export-har` / `POST .../validate` (triage with nestable `path` when available) / `.../gate` / `.../archive` / `.../duplicate` / `.../schedule`
- `GET /api/perf/scenarios/{id}/schedule` — server-computed next fire time + current lease owner; `.../schedule/history` for the fire history
- `GET /api/perf/schedules` — all enabled schedules with next fire times; `/api/perf/schedules/history` across scenarios
- `GET /api/perf/load-policies` — smooth/sustained/stress/custom → ramp/soak/spike; custom accepts `schedule.curve` with `curve_mode=vus|arrivals`
- `POST /api/perf/runs` — `{scenario_id, dispatch, engine, fanout, profile|policy, workers, schedule}`
- `POST /api/perf/runs/import-jtl` — offline JTL → run + samples
- `POST /api/perf/runs/{id}/metrics` — admin or runner token; server recomputes pass/fail via SLA
- `GET /api/perf/runs/{id}/samples?since=` · `.../gate` · `.../runners` · `.../steps` · `.../report?format=csv`

## Parameterised data (CSV)

`datasets_json.csv` drives a real `CSVDataSet` in the emitted plan, so `${column}` tokens bind at run
time instead of reaching the target as literal text.

```json
{
  "csv": {
    "variableNames": "user,password",
    "delimiter": ",",
    "inline": "alice,secret1\nbob,secret2",
    "recycle": true,
    "share_mode": "all"
  }
}
```

- Inline rows are materialized as `data.csv` next to `plan.jmx` in each worker directory; the emitted
  element points at the relative name, which JMeter resolves against the running `.jmx` directory
  (works for host runs and for containers on a shared volume root).
- `filename` (no `inline`) is passed through verbatim for plans imported from JMX — that path stays
  **runner-local**: the file must already exist where the engine runs.
- Inline wins when both are set, and the plan then references `data.csv`.
- The configured delimiter is used for **both** the `data.csv` write and the emitted `delimiter` prop.
  A single character, or the words `tab` / `semicolon` / `pipe` / `comma`; a literal tab is written to
  the plan as `\t`. Anything longer falls back to `,` with a warning on the response.
- Column names travel in the plan as `variableNames`, never as a header row: the generated `data.csv`
  holds data rows only. A first row that matches the declared names is treated as a header and dropped;
  with no declared names the first row *becomes* the names.

### Defaults and why

| Prop | Default | Reason |
|------|---------|--------|
| `recycle` | `true` | A load run outlives the data file; wrapping keeps threads working instead of dying mid-run. |
| `stopThread` | `false` | With recycle on, EOF never happens; stopping threads on EOF would silently shrink the configured VU count and skew the result. Setting both drops `stop_thread` with a warning. |
| `shareMode` | `shareMode.all` | One iterator shared by every thread (and every arrivals segment), so rows are handed out round-robin instead of every thread replaying row 1. `group` / `thread` are accepted. |
| `quotedData` | `true` | The generated `data.csv` is written with RFC 4180 quoting, so values containing the delimiter survive. |
| `ignoreFirstLine` | `false` | The generated file has no header row. Honoured as configured for external `filename` datasets. |
| `fileEncoding` | `UTF-8` | Overridable with `encoding`. |

The element is emitted once at Test Plan level, before the first thread group, so classic VU plans and
arrivals segments share the same iterator.

### Worker fan-out

With `workers > 1` and at least as many rows as workers, rows are sharded round-robin so each row is
used once per pass across the fleet; with fewer rows than workers every worker gets the full file and
rows repeat. The run response says which happened (`dataset_sharded`, `dataset_honesty`).

### Unbound `${…}` tokens

`POST .../scenarios/{id}/validate` cross-checks every `${…}` reference against dataset columns,
extractor refnames, ForEach loop variables, `Argument.name` entries in stored raw JMX, and plan
built-ins (`LOAD_RUN_ID`). Anything left over lands in `unbound_variables[]` with a
`severity: unbound_variable` triage entry, and validation **fails** — a plan that would fire literal
`${…}` text must not report a clean pass or burn engine time. JMeter function calls (`${__P(…)}`,
`${__jexl3(…)}`) are never reported. Dispatch responses carry the same `unbound_variables[]` as a
warning. When a dataset points at an external `filename` with no declared `variableNames`, columns are
unknown here and the response says so (`dataset_columns_unknown`) instead of guessing.

Validate also seeds its 1 VU dry-run with the **first data row**, so parameterised requests are
actually exercised rather than sent with placeholders.

The dry-run only sends requests for steps that *are* requests. Logic controllers
(`if` / `while` / `loop` / `foreach`) are reported as journey structure with their condition, loop count or
input variable and issue nothing — see
[perf-lab.md](perf-lab.md#nested-steps--visual-editor-backend). So the request count a validate puts on the
target is the number of HTTP steps in the journey, not the number of nodes in its tree. Step `headers`
(map or `[{name,value}]`) are applied on that dry-run; empty header maps skip emitting an empty
`HeaderManager`.

## Scenario editor field ↔ JMX matrix

Canonical mapping for the visual scenario editor → stored JSON → emitted Apache JMeter plan.
Emit source of truth: `jmeter_engine.go` (`appendStepJMXIndexed`, `writeJMXStepHeaderManager`),
`jmeter_datasets.go` (`writeJMXCSVDataSet`), SLA gate `harden.go` (`evaluateSLAFailClosed`).
Import reverse-mapping (best-effort) lives in `jmeter_tree.go` (`jmxElementToStep`).

Nested `children` open nested `hashTree`s. Plan-level OPA correlation headers
(`X-OPA-Load-Run-Id` / `baggage`) are a separate ThreadGroup `HeaderManager`
(`writeJMXCorrelationHeaders`) and are **not** the same as per-step `headers`.
No Advanced control without emit; no silent hardcode of a user-visible field.

### HTTP (`steps_json` type `http` → `HTTPSamplerProxy`)

| Editor / JSON field | JMX element / property | Emit notes |
|---|---|---|
| `method`, `url` (→ domain/port/protocol/path), `body` | `HTTPSampler.*` | Body only when non-empty → `HTTPSampler.postBodyRaw` + `HTTPArgument` |
| `headers` (map **or** `[{name,value}]`) | Child `HeaderManager` (`Header.name` / `Header.value`) | Under sampler `hashTree`, **before** extract/assert. Empty → no step HeaderManager |
| `follow_redirects` | `HTTPSampler.follow_redirects` | Default **true** when omitted |
| `connect_timeout_ms` | `HTTPSampler.connect_timeout` | Emitted only when **> 0** |
| `response_timeout_ms` | `HTTPSampler.response_timeout` | Emitted only when **> 0** |
| `always_encode` | `HTTPArgument.always_encode` | Only when body set; default **false** |
| `think_ms` / `think_ms_rand` | `ConstantTimer` and/or `UniformRandomTimer` | When `think_ms_rand > think_ms`: delay = think, range = rand − think. Else if `think_ms > 0`: ConstantTimer only |
| `enabled` | XML `enabled="true\|false"` | Default **true**. Validate **skips** `enabled=false`; JMX still emits disabled |
| `selector_type` / `selector` / `page_url` / `ui_action` | `<!-- opa-ui … -->` comment | Correlation metadata only |

### Extract (`type: extract`)

| Editor / JSON field | JMX (regex) | JMX (jsonpath) | Defaults |
|---|---|---|---|
| `var` | `RegexExtractor.refname` | `JSONPostProcessor.referenceNames` | — |
| `expression` | `RegexExtractor.regex` | `JSONPostProcessor.jsonPathExprs` | jsonpath when `engine=="jsonpath"` **or** expr starts with `$.` |
| `match_number` | `RegexExtractor.match_number` | `JSONPostProcessor.match_numbers` | **1** |
| `template` | `RegexExtractor.template` | *(n/a)* | **`$1$`** (regex only) |
| `default_value` | `RegexExtractor.default` | `JSONPostProcessor.defaultValues` | `""` |

### Assert (`type: assert` → `ResponseAssertion`)

| Editor / JSON field | JMX property | Notes |
|---|---|---|
| `status` / `body_contains` | `Asserion.test_strings` + field/type | Historical spelling **`Asserion.test_strings`**. Status defaults: field `response_code`, type **8** (equals). Body defaults: field `response_data`, type **2** |
| `assert_type` | `Assertion.test_type` | `contains`→1, `equals`→8, `regex`\|`matches`→2 |
| `assert_field` | `Assertion.test_field` | `response_code` / `response_data` / `response_headers` |
| `assume_success` | `Assertion.assume_success` | Default **false** |

### Controllers

| Type | Editor / JSON field | JMX property | Default |
|---|---|---|---|
| Transaction | `include_timers` | `TransactionController.includeTimers` | **false** |
| Transaction | `generate_parent_sample` | `TransactionController.parent` | **false** |
| If | `condition` | `IfController.condition` | `${__jexl3(true)}` if empty |
| If | `evaluate_all` | `IfController.evaluateAll` | **false** |
| If | `use_expression` | `IfController.useExpression` | **true** |
| ForEach | `input_var` / `return_var` | `ForeachController.inputVal` / `returnVal` | `items` / `item` |
| ForEach | `use_separator` | **`ForeachController.useSeparator`** | **true** |

### CSV Advanced (`datasets_json.csv` → `CSVDataSet`)

Emitted once at Test Plan level before the first ThreadGroup. See [Parameterised data (CSV)](#parameterised-data-csv) for delimiter / inline / filename honesty.

| Editor / JSON field (aliases) | JMX property | Default |
|---|---|---|
| `share_mode` / `shareMode` (`all`\|`group`\|`thread`) | `shareMode` | `shareMode.all` |
| `quoted` / `quotedData` | `quotedData` | **true** |
| `ignore_first_line` / `ignoreFirstLine` | `ignoreFirstLine` | **false** (forced false for engine-written inline `data.csv`) |
| `encoding` / `fileEncoding` | `fileEncoding` | `UTF-8` |
| `stop_thread` / `stopThread` | `stopThread` | **false**; if both recycle and stop_thread → stop cleared + warning |
| `recycle` | `recycle` | **true** |

### ThreadGroup / SLA

| Field | Target | Notes |
|---|---|---|
| `schedule.ramp_seconds` | `ThreadGroup.ramp_time` | When **> 0**; else classic group uses **10** |
| `sla.p95_ms` / `error_rate_max` / `rps_min` | Gate / `/gate` (not JMX) | Fail-closed: missing summary fields or breach (`rps < rps_min`) |

### HAR import vs run-time URL policy

| Stage | Lab RFC1918 / loopback / `host.docker.internal` | Cloud metadata |
|---|---|---|
| **Import** | **Kept** with warnings + `private` tally | **Blocked** |
| **Validate / dispatch** | Still blocked unless listed in **`OPA_PERF_INTERNAL_HOSTS`** | Still blocked |

### Validate (`POST …/scenarios/{id}/validate`)

| Behavior | Detail |
|---|---|
| **Admin-only** | `perfRequireAdmin` when auth enforced; viewers **403** |
| **Step headers → target** | Dry-run applies `stepHTTPHeaderPairs` |
| **`unbound_variables` fail-closed** | Unresolved plain `${name}` → `pass=false`; JMeter function forms not reported |
| **`path[]` triage** | Nestable index path on triage / correlation |
| Disabled steps | `enabled=false` omitted from dry-run; still in JSON / JMX |

`export-jmx` stays view-scoped (not admin-gated).

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

Full JMeter plugin fidelity, multi-cloud public generators, real-browser hybrid VUs, auto-fix pull requests, JVM/.NET agents, Kubernetes Job runner (interface reserved).

## Security notes

- Scenario upsert / import-jmx / import-har / import-xhr / import-postman / validate / archive / duplicate require **editor** (or admin) when `OPA_AUTH_REQUIRED=1`.
- Personal accounts: scenario rows store `user_id` (`WriteOwner`); by-id load/validate use `OwnedRowPredicate` on that owner. Org lists of `load_scenarios` exclude personal rows via `ExcludePersonalRows`.
- Run **dispatch / fan-out** and **cancel** still require **admin**.
- Metrics POST requires **admin** or `OPA_PERF_RUNNER_TOKEN` (viewers cannot forge pass/fail).
- Viewers may list scenarios and create undispatched run IDs for correlation; export-jmx/xhr/har are view-scoped.
- Validate uses dial-pinned HTTP (DNS rebinding resistant) and treats only **2xx** as OK.
- HAR/XHR import **keeps** lab private / loopback / `host.docker.internal` with warnings and still **blocks** cloud metadata; validate/dispatch dial-pin is unchanged (`OPA_PERF_INTERNAL_HOSTS`).
- Scenario gate rejects runs whose `scenario_id` does not match the URL.
- SLA gate is fail-closed (empty/running summaries and empty SLA fail unless `OPA_PERF_ALLOW_EMPTY_SLA=1`).
