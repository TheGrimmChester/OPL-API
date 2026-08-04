# Changelog

## Unreleased

- Auth: adopt Open-Auth-Go per-user project ACLs (`project_ids` / `EnforceProjectACL` on Gate middleware). Restricted JWTs get **403** on non-member `X-Project-ID`; role `admin` stays unrestricted. No second membership store — hub-minted claims only.
- Run lifecycle: `POST /api/perf/runs` writes `created` when undispatched and `failed` when dispatch errors (no more stuck `running`); `POST /api/perf/runs/{id}/cancel`.
- SLA gate JSON includes `pass` alongside `ok`/`status`.
- Docs: tenant headers required for `GET /api/perf/scenarios` and `GET /api/perf/runs` when auth is on; NAS curl examples in interop/perf-lab.
- Auth via Open-Auth-Go `Gate` (delete local `auth.go` / `auth_local.go` duplicates).
- Qualify Perf Lab tables with `CLICKHOUSE_DB` via `chTable()`; hub tenant tables use `hubTable()` (`opa`).
- Create `load_*` schema in the product database on startup.

- Product branding: Open Perf Lab (`opl-api` / `OPL-API`).
