# Optional micro-services

```text
opl-gateway
  ├── opl-scenarios
  └── opl-runs
opl-orchestrator   # already peeled: leased schedule dispatch + run reaper (:8097)
opl-runner-jmeter
```

`opl-orchestrator` is the one service on this list that already runs as its own process. It shares
ClickHouse and the schedule lease table with `opl-api` rather than owning a private queue, so it can be
scaled to more than one replica without double-firing — see
[architecture.md](architecture.md#schedule-leasing). It exposes `/api/health`,
`/api/orchestrator/state`, and the read-only schedule list.

Compose stub (comments only until peeled):

```yaml
# opl-gateway:
#   image: opl-gateway:nas
#   ports: ["8092:8092"]
```
