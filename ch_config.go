package main

import (
	"os"
	"strings"
)

// clickHouseDatabase resolves the product ClickHouse database name.
// Precedence: CLICKHOUSE_DB > CLICKHOUSE_DATABASE > product default.
func clickHouseDatabase() string {
	if v := strings.TrimSpace(os.Getenv("CLICKHOUSE_DB")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CLICKHOUSE_DATABASE")); v != "" {
		return v
	}
	return defaultClickHouseDB
}

// ensureClickHouseDatabase creates the product DB when a query client is available.
func ensureClickHouseDatabase(q *ClickHouseQuery) {
	if q == nil {
		return
	}
	db := q.database
	if db == "" || db == "default" {
		return
	}
	_ = q.Execute("CREATE DATABASE IF NOT EXISTS " + db)
}
