module github.com/TheGrimmChester/opl-api

go 1.25.0

// The family libraries were wired with filesystem replaces (=> ../Open-Auth-Go).
// Those resolve in a developer tree but not in a single-repo checkout, so every
// CI checkup aborted before a test ran:
//
//	helpers.go:8:2: …/open-http-go@v0.0.0-…: replacement directory
//	../Open-HTTP-Go does not exist
//
// The versions below come from the public module proxy instead, so this go.mod
// resolves anywhere. To develop against live sibling checkouts, create a
// go.work (gitignored) — see docs/go-modules.md.
require (
	github.com/TheGrimmChester/open-auth-go v0.0.0-20260805140119-f8f106b388fb
	github.com/TheGrimmChester/open-clickhouse-go v0.2.0
	github.com/TheGrimmChester/open-http-go v0.0.0-20260804055231-a9462e336412
	github.com/TheGrimmChester/open-job-go v0.0.0-20260803091535-04d163946627
	github.com/TheGrimmChester/open-logger-go v0.2.0
	github.com/TheGrimmChester/open-tenant-go v0.2.2
)

require github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
