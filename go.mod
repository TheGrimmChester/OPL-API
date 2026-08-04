module github.com/TheGrimmChester/opl-api

go 1.25.0

require (
	github.com/TheGrimmChester/open-auth-go v0.0.0
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
)

require (
	github.com/TheGrimmChester/open-clickhouse-go v0.2.0
	github.com/TheGrimmChester/open-job-go v0.0.0
	github.com/TheGrimmChester/open-tenant-go v0.2.0
)

replace github.com/TheGrimmChester/open-job-go => ../Open-Job-Go

replace github.com/TheGrimmChester/open-auth-go => ../Open-Auth-Go

replace github.com/TheGrimmChester/open-tenant-go => ../Open-Tenant-Go

replace github.com/TheGrimmChester/open-clickhouse-go => ../Open-ClickHouse-Go
