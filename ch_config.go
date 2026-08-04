package main

import (
	"log"
	"os"
	"strings"
)

// hubClickHouseDB is the shared OPA tenant directory (orgs, projects, API keys, federation).
// Product tables live in clickHouseDatabase(); never rewrite hub queries into the product DB.
const hubClickHouseDB = "opa"

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

// chTable returns a fully-qualified product table (e.g. opl.load_runs).
func chTable(name string) string {
	return clickHouseDatabase() + "." + name
}

// hubTable returns a fully-qualified hub table (always opa.<name>).
func hubTable(name string) string {
	return hubClickHouseDB + "." + name
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

// ensurePerfLabSchema creates OPL load tables in the product database.
func ensurePerfLabSchema(q *ClickHouseQuery) {
	if q == nil {
		return
	}
	db := clickHouseDatabase()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_scenarios (
			id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			name String,
			target_url String,
			method LowCardinality(String) DEFAULT 'GET',
			vus UInt32 DEFAULT 1,
			duration_seconds UInt32 DEFAULT 30,
			headers_json String DEFAULT '{}',
			body String DEFAULT '',
			thresholds_json String DEFAULT '{}',
			created_at DateTime64(3) DEFAULT now64(3),
			updated_at DateTime64(3) DEFAULT now64(3),
			steps_json String DEFAULT '[]',
			datasets_json String DEFAULT '{}',
			sla_json String DEFAULT '{}',
			schedule_json String DEFAULT '{}',
			jmx_xml String DEFAULT '',
			archived UInt8 DEFAULT 0
		) ENGINE = ReplacingMergeTree(updated_at)
		ORDER BY (organization_id, project_id, id)`,
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_runs (
			id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			scenario_id String DEFAULT '',
			status LowCardinality(String) DEFAULT 'running',
			vus UInt32 DEFAULT 1,
			started_at DateTime64(3) DEFAULT now64(3),
			finished_at DateTime64(3) DEFAULT now64(3),
			summary_json String DEFAULT '{}',
			error String DEFAULT ''
		) ENGINE = ReplacingMergeTree(finished_at)
		ORDER BY (organization_id, project_id, id)`,
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_run_samples (
			run_id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			ts DateTime64(3) DEFAULT now64(3),
			latency_ms Float64 DEFAULT 0,
			status_code UInt16 DEFAULT 0,
			ok UInt8 DEFAULT 0,
			url String DEFAULT '',
			step_name String DEFAULT ''
		) ENGINE = MergeTree
		PARTITION BY toDate(ts)
		ORDER BY (organization_id, project_id, run_id, ts)
		TTL toDateTime(ts) + toIntervalDay(30)`,
	}
	for _, s := range stmts {
		if err := q.Execute(s); err != nil {
			log.Printf("perf schema: %v", err)
		}
	}
	// Existing NAS tables may predate archived; ADD COLUMN is idempotent.
	if err := q.Execute(`ALTER TABLE ` + db + `.load_scenarios ADD COLUMN IF NOT EXISTS archived UInt8 DEFAULT 0`); err != nil {
		log.Printf("perf schema archived column: %v", err)
	}
}
