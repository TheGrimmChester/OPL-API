# OPA Perf Lab

Owns Wave 29/31 load scenarios, runs, HAR/XHR/JMX import, Docker JMeter engine.

| Port (smoke) | Service |
|---|---|
| **8092** | This service |
| 8080 | OPA-Agent |
| 8091 | OPA-AI-Orchestrator |

**Shared:** ClickHouse (`CLICKHOUSE_URL`), JWT (`JWT_SECRET` — same secret as Agent).

**Not here:** `/api/performance/*` baselines/gate/insights (still on Agent), SCM/security orchestration.

Dashboard routes `/api/perf/*` here (via `VITE_PERF_LAB_URL` or nginx path proxy).
