# OPL-API

Go API for **Open Perf Lab** — load scenarios, runs, HAR/JMX import, and Docker JMeter engine.

| Port (smoke) | Service |
|---|---|
| **8092** | `opl-api` |
| 8080 | `opa-hub` / `opa-agent` |
| 8091 | `ora-api` |
| 8093 | `osa-api` |

**Shared when co-deployed:** ClickHouse (`CLICKHOUSE_URL`), user JWT (`JWT_SECRET`).

**Not here:** APM UI (**OPA**), review (**ORA**), AppSec (**OSA**).

## Documentation

See [docs/index.md](docs/index.md).

## Build

```bash
go build -o opl-api .
```

Image tags: `opl-api:smoke` (laptop) · `opl-api:nas` (production / NAS only).
