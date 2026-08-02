# OPA Perf Lab

Owns load scenarios, runs, HAR/XHR/JMX import, and Docker JMeter engine.

| Port (smoke) | Service |
|---|---|
| **8092** | This service |
| 8080 | OPA-Agent |
| 8091 | OPA-AI-Orchestrator |

**Shared:** ClickHouse (`CLICKHOUSE_URL`), JWT (`JWT_SECRET` — same secret as Agent).

Dashboard `/api/perf/*` routes here (via `VITE_PERF_LAB_URL` or nginx path proxy).
