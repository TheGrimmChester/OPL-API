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
			user_id String DEFAULT '',
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
		// Saved report / trend layouts (widgets, metrics, window) per org+project.
		`CREATE TABLE IF NOT EXISTS ` + db + `.report_templates (
			id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			name String,
			kind LowCardinality(String) DEFAULT 'report',
			widgets_json String DEFAULT '[]',
			metrics_json String DEFAULT '[]',
			window_json String DEFAULT '{}',
			options_json String DEFAULT '{}',
			archived UInt8 DEFAULT 0,
			created_at DateTime64(3) DEFAULT now64(3),
			updated_at DateTime64(3) DEFAULT now64(3)
		) ENGINE = ReplacingMergeTree(updated_at)
		ORDER BY (organization_id, project_id, id)`,
		// One row per notification channel attempt on a terminal run — including
		// channels skipped because they are not configured (never silent).
		`CREATE TABLE IF NOT EXISTS ` + db + `.run_notifications (
			id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			run_id String DEFAULT '',
			scenario_id String DEFAULT '',
			run_status LowCardinality(String) DEFAULT '',
			channel LowCardinality(String) DEFAULT '',
			result LowCardinality(String) DEFAULT '',
			target String DEFAULT '',
			detail String DEFAULT '',
			mode LowCardinality(String) DEFAULT '',
			source LowCardinality(String) DEFAULT '',
			created_at DateTime64(3) DEFAULT now64(3)
		) ENGINE = MergeTree
		PARTITION BY toDate(created_at)
		ORDER BY (organization_id, project_id, created_at, run_id)
		TTL toDateTime(created_at) + toIntervalDay(90)`,
	}
	for _, s := range stmts {
		if err := q.Execute(s); err != nil {
			log.Printf("perf schema: %v", err)
		}
	}
	// Existing NAS tables may predate archived / personal owner; ADD COLUMN is idempotent.
	if err := q.Execute(`ALTER TABLE ` + db + `.load_scenarios ADD COLUMN IF NOT EXISTS archived UInt8 DEFAULT 0`); err != nil {
		log.Printf("perf schema archived column: %v", err)
	}
	if err := q.Execute(`ALTER TABLE ` + db + `.load_scenarios ADD COLUMN IF NOT EXISTS user_id String DEFAULT ''`); err != nil {
		log.Printf("perf schema user_id column: %v", err)
	}
	ensureScheduleSchema(q)
}

// ensureScheduleSchema creates the scheduling tables: state, leases, fire history.
//
// Additive only — load_scenarios is untouched. Scheduling state moves OUT of
// load_scenarios.schedule_json into load_schedule_state so recording a fire no
// longer rewrites the whole scenario row (which silently discarded a scenario
// edit made concurrently with a fire).
func ensureScheduleSchema(q *ClickHouseQuery) {
	if q == nil {
		return
	}
	db := clickHouseDatabase()
	stmts := []string{
		// Per-scenario scheduling state. One narrow row per scenario, so a fire
		// writes 9 scheduling columns instead of replacing the scenario definition.
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_schedule_state (
			scenario_id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			last_fired_at DateTime64(3) DEFAULT toDateTime64(0, 3),
			next_fire_at DateTime64(3) DEFAULT toDateTime64(0, 3),
			last_run_id String DEFAULT '',
			last_fire_key String DEFAULT '',
			last_owner String DEFAULT '',
			fire_count UInt64 DEFAULT 0,
			updated_at DateTime64(3) DEFAULT now64(3)
		) ENGINE = ReplacingMergeTree(updated_at)
		ORDER BY (organization_id, project_id, scenario_id)`,
		// One row per lease claim on one scheduled occurrence (fire_key).
		// Append-only on purpose: the winner is arbitrated by reading the claim
		// set back, which makes every contention decision auditable after the
		// fact, and lets an expired owner be taken over instead of wedging.
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_schedule_leases (
			scenario_id String,
			fire_key String,
			owner String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			claimed_at DateTime64(3) DEFAULT now64(3),
			expires_at DateTime64(3) DEFAULT now64(3),
			generation UInt32 DEFAULT 0,
			released UInt8 DEFAULT 0,
			run_id String DEFAULT ''
		) ENGINE = MergeTree
		PARTITION BY toDate(claimed_at)
		ORDER BY (organization_id, project_id, scenario_id, fire_key, claimed_at, owner)
		TTL toDateTime(claimed_at) + toIntervalDay(30)`,
		// Auditable fire history: every occurrence the scheduler acted on, the
		// owner that won the lease, and what happened to the dispatch.
		`CREATE TABLE IF NOT EXISTS ` + db + `.load_schedule_fires (
			id String,
			scenario_id String,
			organization_id String DEFAULT '',
			project_id String DEFAULT '',
			fire_key String DEFAULT '',
			owner String DEFAULT '',
			run_id String DEFAULT '',
			outcome LowCardinality(String) DEFAULT '',
			vus UInt32 DEFAULT 0,
			detail String DEFAULT '',
			source LowCardinality(String) DEFAULT '',
			next_fire_at DateTime64(3) DEFAULT toDateTime64(0, 3),
			fired_at DateTime64(3) DEFAULT now64(3)
		) ENGINE = MergeTree
		PARTITION BY toDate(fired_at)
		ORDER BY (organization_id, project_id, scenario_id, fired_at)
		TTL toDateTime(fired_at) + toIntervalDay(90)`,
	}
	for _, s := range stmts {
		if err := q.Execute(s); err != nil {
			log.Printf("schedule schema: %v", err)
		}
	}
}
