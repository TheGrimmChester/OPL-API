# Changelog

## Unreleased

- Docs: tenant headers required for `GET /api/perf/scenarios` and `GET /api/perf/runs` when auth is on; NAS curl examples in interop/perf-lab.
- Auth via Open-Auth-Go `Gate` (delete local `auth.go` / `auth_local.go` duplicates).
- Qualify Perf Lab tables with `CLICKHOUSE_DB` via `chTable()`; hub tenant tables use `hubTable()` (`opa`).
- Create `load_*` schema in the product database on startup.

- Product branding: Open Perf Lab (`opl-api` / `OPL-API`).
