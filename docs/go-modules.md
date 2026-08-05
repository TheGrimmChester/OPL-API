# Go modules and the family libraries

OPL-API depends on shared libraries that live in their own repositories:

| Module | Repository |
| --- | --- |
| `open-auth-go` | `TheGrimmChester/Open-Auth-Go` |
| `open-clickhouse-go` | `TheGrimmChester/Open-ClickHouse-Go` |
| `open-http-go` | `TheGrimmChester/Open-HTTP-Go` |
| `open-job-go` | `TheGrimmChester/Open-Job-Go` |
| `open-logger-go` | `TheGrimmChester/Open-Logger-Go` |
| `open-tenant-go` | `TheGrimmChester/Open-Tenant-Go` |

They are all public, so `go.mod` pins them at proxy-resolvable versions and the
repository builds from a checkout of OPL-API alone.

## Why the filesystem replaces are gone

`go.mod` used to wire each library to a sibling directory:

```
replace github.com/TheGrimmChester/open-auth-go => ../Open-Auth-Go
```

That works in a developer tree, where all the repositories sit side by side. It
cannot work in CI, which checks out one repository. The OPA Checkup `go-test`
step failed on every commit with one error per library:

```
auth_wire.go:7:2: github.com/TheGrimmChester/open-auth-go@v0.0.0:
	replacement directory ../Open-Auth-Go does not exist
```

The failure was reported as a red check, so the signal said "this commit is
broken" when the commit was fine and the runner layout was not. Pinning real
versions removes the dependency on the checkout layout entirely.

## Local development against live sibling checkouts

Editing a library and seeing the change immediately still works — through a
workspace file rather than `go.mod`:

```bash
./scripts/dev-go-work.sh
```

That writes a `go.work` listing this repository plus its siblings. `go.work` is
gitignored on purpose: if it were committed, CI would fail exactly the way it did
before. Point the script somewhere else with `FAMILY_ROOT`:

```bash
FAMILY_ROOT=~/src ./scripts/dev-go-work.sh
```

To build against the pinned versions again — which is what CI does — remove the
file or set `GOWORK=off`:

```bash
rm go.work
```

```bash
GOWORK=off go test ./...
```

## Bumping a library

A library change reaches OPL-API only once it is pushed and the version here is
bumped:

```bash
go get github.com/TheGrimmChester/open-logger-go@main && go mod tidy
```

Use a tag instead of `@main` where one exists: `open-clickhouse-go`; `open-logger-go`; `open-tenant-go` are tagged, the rest resolve to commit pseudo-versions.
