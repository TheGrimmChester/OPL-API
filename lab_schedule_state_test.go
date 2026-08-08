package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fake ClickHouse ---
//
// Enough of the HTTP interface for the scheduling code to run against its real
// SQL and its real JSONEachRow marshalling: INSERTs are captured per table, and
// SELECTs are answered from those captured rows. It emulates only what the
// scheduling queries actually use — equality filters, LIMIT, first-key ORDER BY,
// toUnixTimestamp64Milli() projections, and FINAL collapsing to the last row per
// key for the ReplacingMergeTree tables.

var (
	reTable       = regexp.MustCompile(`(?is)\bFROM\s+(\w+)\.(\w+)`)
	reInsertTable = regexp.MustCompile(`(?is)INSERT\s+INTO\s+(\w+)\.(\w+)`)
	reEq          = regexp.MustCompile(`(?is)(\w+)\s*=\s*'([^']*)'`)
	reMilli       = regexp.MustCompile(`(?is)toUnixTimestamp64Milli\((\w+)\)\s+AS\s+(\w+)`)
	reLimit       = regexp.MustCompile(`(?is)\bLIMIT\s+(\d+)`)
	reOrder       = regexp.MustCompile(`(?is)\bORDER\s+BY\s+(\w+)\s*(ASC|DESC)?`)
)

// replacingKey names the dedup key for the ReplacingMergeTree tables the
// scheduling code reads with FINAL.
var replacingKey = map[string]string{
	"load_scenarios":      "id",
	"load_runs":           "id",
	"load_schedule_state": "scenario_id",
}

type fakeClickHouse struct {
	mu     sync.Mutex
	tables map[string][]map[string]interface{}
	srv    *httptest.Server
}

func newFakeClickHouse(t *testing.T) *fakeClickHouse {
	t.Helper()
	f := &fakeClickHouse{tables: map[string][]map[string]interface{}{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClickHouse) handle(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	upper := strings.ToUpper(query)
	switch {
	case strings.Contains(upper, "INSERT INTO"):
		f.doInsert(query, r)
		w.WriteHeader(200)
	case strings.HasPrefix(strings.TrimSpace(upper), "SELECT"):
		rows, err := f.doSelect(query)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for _, row := range rows {
			b, _ := json.Marshal(row)
			w.Write(append(b, '\n'))
		}
	default:
		// CREATE TABLE / CREATE DATABASE / ALTER: accepted and ignored.
		w.WriteHeader(200)
	}
}

func (f *fakeClickHouse) doInsert(query string, r *http.Request) {
	m := reInsertTable.FindStringSubmatch(query)
	if len(m) < 3 {
		return
	}
	table := m[2]
	dec := json.NewDecoder(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		var row map[string]interface{}
		if err := dec.Decode(&row); err != nil {
			break
		}
		f.tables[table] = append(f.tables[table], row)
	}
}

func (f *fakeClickHouse) doSelect(query string) ([]map[string]interface{}, error) {
	m := reTable.FindStringSubmatch(query)
	if len(m) < 3 {
		return nil, fmt.Errorf("fake clickhouse: cannot find table in %q", query)
	}
	table := m[2]
	f.mu.Lock()
	raw, known := f.tables[table]
	stored := append([]map[string]interface{}(nil), raw...)
	f.mu.Unlock()
	if !known {
		// An unknown table behaves like a pre-migration deployment.
		return nil, fmt.Errorf("fake clickhouse: unknown table %q", table)
	}

	// FINAL on a ReplacingMergeTree: keep the last inserted row per key.
	if strings.Contains(strings.ToUpper(query), "FINAL") {
		if key, ok := replacingKey[table]; ok {
			seen := map[string]int{}
			out := []map[string]interface{}{}
			for _, row := range stored {
				k := fmt.Sprint(row[key])
				if idx, dup := seen[k]; dup {
					out[idx] = row
					continue
				}
				seen[k] = len(out)
				out = append(out, row)
			}
			stored = out
		}
	}

	// Equality filters. Only columns that exist on the row are treated as
	// filters, so SELECT-list aliases and function arguments are ignored.
	filters := map[string]string{}
	whereIdx := strings.Index(strings.ToUpper(query), "WHERE")
	if whereIdx >= 0 {
		for _, eq := range reEq.FindAllStringSubmatch(query[whereIdx:], -1) {
			filters[eq[1]] = eq[2]
		}
	}
	rows := []map[string]interface{}{}
	for _, row := range stored {
		keep := true
		for col, want := range filters {
			if _, present := row[col]; !present {
				continue
			}
			if fmt.Sprint(row[col]) != want {
				keep = false
				break
			}
		}
		if keep {
			rows = append(rows, cloneRow(row))
		}
	}

	// toUnixTimestamp64Milli(col) AS alias
	for _, proj := range reMilli.FindAllStringSubmatch(query, -1) {
		col, alias := proj[1], proj[2]
		for _, row := range rows {
			row[alias] = chMillis(row[col])
		}
	}

	if om := reOrder.FindStringSubmatch(query); len(om) >= 2 {
		col, desc := om[1], strings.EqualFold(om[2], "DESC")
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := fmt.Sprint(rows[i][col]), fmt.Sprint(rows[j][col])
			if desc {
				return a > b
			}
			return a < b
		})
	}
	if lm := reLimit.FindStringSubmatch(query); len(lm) >= 2 {
		if n, err := strconv.Atoi(lm[1]); err == nil && n < len(rows) {
			rows = rows[:n]
		}
	}
	return rows, nil
}

func cloneRow(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// chMillis converts a stored "2006-01-02 15:04:05.000" value to epoch millis,
// matching toUnixTimestamp64Milli.
func chMillis(v interface{}) int64 {
	s := fmt.Sprint(v)
	for _, layout := range []string{"2006-01-02 15:04:05.000", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().UnixMilli()
		}
	}
	return 0
}

func (f *fakeClickHouse) rows(table string) []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]interface{}(nil), f.tables[table]...)
}

func (f *fakeClickHouse) seed(table string, rows ...map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tables[table] = append(f.tables[table], rows...)
}

// ensureTable registers an empty table so SELECTs return no rows instead of
// behaving like a missing table.
func (f *fakeClickHouse) ensureTable(tables ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tbl := range tables {
		if f.tables[tbl] == nil {
			f.tables[tbl] = []map[string]interface{}{}
		}
	}
}

// wireFakeClickHouse points the process-wide writer and query client at the fake
// and restores them afterwards.
func wireFakeClickHouse(t *testing.T) *fakeClickHouse {
	t.Helper()
	f := newFakeClickHouse(t)
	prevW, prevQ := writer, queryClient
	writer = NewClickHouseWriterDB(f.srv.URL, clickHouseDatabase(), 100)
	queryClient = NewClickHouseQueryDB(f.srv.URL, clickHouseDatabase())
	t.Cleanup(func() { writer, queryClient = prevW, prevQ })
	return f
}

func scenarioRow(id, name string, updated time.Time) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "organization_id": "org", "project_id": "proj",
		"name": name, "target_url": "http://target/health", "method": "GET",
		"vus": 5, "duration_seconds": 30,
		"headers_json": "{}", "body": "", "thresholds_json": "{}",
		"steps_json": "[]", "datasets_json": "{}", "sla_json": "{}",
		"schedule_json": `{"enabled":true,"every_minutes":15}`,
		"jmx_xml":       "", "archived": 0,
		"updated_at": updated.Format("2006-01-02 15:04:05.000"),
		"created_at": updated.Format("2006-01-02 15:04:05.000"),
	}
}

// TestFireRecordDoesNotClobberConcurrentScenarioEdit is the regression test for
// the bug this change fixes.
//
// Recording a fire used to rebuild the ENTIRE load_scenarios row from a snapshot
// read before the fire started. A scenario edit committed in between was
// therefore silently reverted by the scheduler. The test drives the real write
// path against a fake ClickHouse and asserts that recording a fire leaves the
// concurrent edit intact.
func TestFireRecordDoesNotClobberConcurrentScenarioEdit(t *testing.T) {
	f := wireFakeClickHouse(t)
	f.ensureTable("load_schedule_state", "load_schedule_fires")
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	// The scheduler reads the scenario at the top of its tick...
	staleSnapshot := scenarioRow("scn-1", "original name", base)
	f.seed("load_scenarios", staleSnapshot)

	// ...an operator edits the scenario while the run is being dispatched...
	edited := scenarioRow("scn-1", "edited while the run was dispatching", base.Add(time.Second))
	edited["vus"] = 42
	f.seed("load_scenarios", edited)

	// ...and only then does the fire get recorded.
	writeScheduleState(scheduleState{
		ScenarioID: "scn-1", Org: "org", Proj: "proj",
		LastFiredAt: base.Add(2 * time.Second),
		NextFireAt:  base.Add(17 * time.Minute),
		LastRunID:   "run-1", LastFireKey: "at:20260804T090000Z",
		LastOwner: "replica-a", FireCount: 1,
	})
	recordScheduleFire(scheduleFireRecord{
		ScenarioID: "scn-1", Org: "org", Proj: "proj",
		FireKey: "at:20260804T090000Z", Owner: "replica-a", RunID: "run-1",
		Outcome: "fired", VUs: 5, FiredAt: base.Add(2 * time.Second),
	})
	flushAsyncWrites(t, f, "load_schedule_state", 1)

	// The edit must survive: the fire path must not have written load_scenarios.
	scenarioWrites := f.rows("load_scenarios")
	if len(scenarioWrites) != 2 {
		t.Fatalf("recording a fire must not write load_scenarios: expected the 2 seeded rows, got %d", len(scenarioWrites))
	}
	latest := scenarioWrites[len(scenarioWrites)-1]
	if got := getString(latest, "name"); got != "edited while the run was dispatching" {
		t.Fatalf("concurrent scenario edit was clobbered: name is now %q", got)
	}
	if got := int(getFloat64(latest, "vus")); got != 42 {
		t.Fatalf("concurrent scenario edit was clobbered: vus is now %d, want 42", got)
	}

	// And the fire really was recorded — in its own tables.
	state := f.rows("load_schedule_state")
	if len(state) != 1 {
		t.Fatalf("expected exactly 1 schedule state row, got %d", len(state))
	}
	if got := getString(state[0], "last_run_id"); got != "run-1" {
		t.Fatalf("state row lost the run id: %q", got)
	}
	if got := getString(state[0], "last_owner"); got != "replica-a" {
		t.Fatalf("state row lost the lease owner: %q", got)
	}
	fires := f.rows("load_schedule_fires")
	if len(fires) != 1 {
		t.Fatalf("expected exactly 1 fire history row, got %d", len(fires))
	}
	if got := getString(fires[0], "fire_key"); got != "at:20260804T090000Z" {
		t.Fatalf("fire history lost the fire key: %q", got)
	}
}

// TestFullRowRewriteClobbersConcurrentEdit is the negative control for the test
// above: it drives the real full-row rewrite (bumpScenarioSchedule, the path the
// fire no longer takes) with a stale snapshot and shows the edit being reverted.
// Without this, "the fire did not clobber the edit" could just mean the test set
// up no clobber to begin with.
func TestFullRowRewriteClobbersConcurrentEdit(t *testing.T) {
	f := wireFakeClickHouse(t)
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	staleSnapshot := scenarioRow("scn-1", "original name", base)
	f.seed("load_scenarios", staleSnapshot)
	edited := scenarioRow("scn-1", "edited while the run was dispatching", base.Add(time.Second))
	edited["vus"] = 42
	f.seed("load_scenarios", edited)

	// queryClient is deliberately unset here so bumpScenarioSchedule falls back
	// to the caller's stale snapshot, which is exactly what the scheduler used
	// to hand it.
	prevQ := queryClient
	queryClient = nil
	t.Cleanup(func() { queryClient = prevQ })

	sched := map[string]interface{}{"enabled": true, "every_minutes": 15}
	bumpScenarioSchedule("scn-1", "org", "proj", "", staleSnapshot, sched)
	flushAsyncWrites(t, f, "load_scenarios", 3)

	rows := f.rows("load_scenarios")
	latest := rows[len(rows)-1]
	if got := getString(latest, "name"); got != "original name" {
		t.Fatalf("control failed: expected the full-row rewrite to revert the name, got %q", got)
	}
	if got := int(getFloat64(latest, "vus")); got != 5 {
		t.Fatalf("control failed: expected the full-row rewrite to revert vus to 5, got %d", got)
	}
}

// flushAsyncWrites waits for insertAsync goroutines to land, so a test never
// asserts on a write that simply has not arrived yet.
func flushAsyncWrites(t *testing.T, f *fakeClickHouse, table string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.rows(table)) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d rows in %s (have %d)", want, table, len(f.rows(table)))
}

// TestScheduleStateRoundTrip proves scheduling state survives a real write and
// read through the new table, including the epoch-millis timestamp projection.
func TestScheduleStateRoundTrip(t *testing.T) {
	f := wireFakeClickHouse(t)
	f.ensureTable("load_schedule_state")
	last := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	next := time.Date(2026, 8, 4, 9, 15, 0, 0, time.UTC)
	writeScheduleState(scheduleState{
		ScenarioID: "scn-1", Org: "org", Proj: "proj",
		LastFiredAt: last, NextFireAt: next,
		LastRunID: "run-1", LastFireKey: "k1", LastOwner: "replica-a", FireCount: 7,
	})
	flushAsyncWrites(t, f, "load_schedule_state", 1)

	got := loadScheduleStateFor("scn-1", "org", "proj")
	if !got.LastFiredAt.Equal(last) {
		t.Fatalf("last_fired_at round trip: got %v want %v", got.LastFiredAt, last)
	}
	if !got.NextFireAt.Equal(next) {
		t.Fatalf("next_fire_at round trip: got %v want %v", got.NextFireAt, next)
	}
	if got.FireCount != 7 || got.LastOwner != "replica-a" || got.LastRunID != "run-1" {
		t.Fatalf("state round trip lost fields: %+v", got)
	}
	if got.empty() {
		t.Fatalf("a populated state row must not report empty")
	}

	all := loadScheduleStates()
	if len(all) != 1 || all["scn-1"].FireCount != 7 {
		t.Fatalf("batch state read returned %d rows: %+v", len(all), all)
	}
}

// TestScheduleStateMissingTableDegrades pins the pre-migration behaviour: no
// state table means an empty state, and the scheduler falls back to the values
// still in schedule_json rather than stopping.
func TestScheduleStateMissingTableDegrades(t *testing.T) {
	f := wireFakeClickHouse(t)
	_ = f
	got := loadScheduleStateFor("scn-1", "org", "proj")
	if !got.empty() {
		t.Fatalf("missing state table should yield an empty state, got %+v", got)
	}
	if len(loadScheduleStates()) != 0 {
		t.Fatalf("missing state table should yield no states")
	}
	// The legacy schedule_json path still decides due-ness.
	sched := map[string]interface{}{"enabled": true, "next_fire_at": "2026-08-04T09:00:00Z"}
	if !scheduleDueWithState(sched, got, time.Date(2026, 8, 4, 9, 0, 1, 0, time.UTC)) {
		t.Fatalf("with no state row, schedule_json.next_fire_at must still make the schedule due")
	}
}

// TestLeaseStoreRoundTripThroughClickHouse exercises the production lease store
// end to end: real INSERT marshalling, real SELECT, real millisecond projection.
func TestLeaseStoreRoundTripThroughClickHouse(t *testing.T) {
	f := wireFakeClickHouse(t)
	f.ensureTable("load_schedule_leases")
	store := chScheduleLeaseStore{}
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	if err := store.InsertClaim(scheduleClaim{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-a", Org: "org", Proj: "proj",
		ClaimedAt: now, ExpiresAt: now.Add(5 * time.Minute), Generation: 2,
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	claims, err := store.ListClaims("scn-1", "k1")
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(claims))
	}
	c := claims[0]
	if !c.ClaimedAt.Equal(now) || !c.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("claim timestamps did not round trip: %+v", c)
	}
	if c.Owner != "replica-a" || c.Generation != 2 || c.Released {
		t.Fatalf("claim fields did not round trip: %+v", c)
	}
	// A different occurrence must not see this claim, or every occurrence would
	// contend on one lease.
	other, err := store.ListClaims("scn-1", "k2")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("claims must be scoped per fire key, got %d for k2", len(other))
	}
}

// TestTwoSchedulersThroughClickHouseFireOnce is the concurrency guarantee run
// through the production lease store rather than an in-memory double: real SQL,
// real JSON, real millisecond truncation.
func TestTwoSchedulersThroughClickHouseFireOnce(t *testing.T) {
	f := wireFakeClickHouse(t)
	f.ensureTable("load_schedule_leases")
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	winners := runContenders(t, 2, func(req scheduleClaimRequest) (bool, error) {
		res, err := claimScheduleFire(chScheduleLeaseStore{}, req)
		return res.Acquired, err
	}, now, "at:20260804T100000Z")
	if len(winners) != 1 {
		t.Fatalf("two schedulers over ClickHouse must yield exactly 1 fire, got %d: %v", len(winners), winners)
	}
	if got := len(f.rows("load_schedule_leases")); got != 2 {
		t.Fatalf("expected both claims on record for audit, got %d", got)
	}
}

// TestScheduleStatusInfoExposesServerSideNextFire pins the contract the UI reads
// so it never has to compute a next fire time itself.
func TestScheduleStatusInfoExposesServerSideNextFire(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	sched := map[string]interface{}{"enabled": true, "every_minutes": 15}

	// From the state row: authoritative.
	st := scheduleState{
		ScenarioID: "scn-1", NextFireAt: now.Add(5 * time.Minute),
		LastFiredAt: now.Add(-10 * time.Minute), LastRunID: "run-1",
		LastOwner: "replica-a", FireCount: 3,
	}
	got := scheduleStatusInfo(sched, st, now)
	if got["next_fire_at"] != now.Add(5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("next_fire_at should come from the state row, got %v", got["next_fire_at"])
	}
	if got["next_fire_source"] != "state_row" {
		t.Fatalf("expected next_fire_source=state_row, got %v", got["next_fire_source"])
	}
	if got["next_fire_in_seconds"] != 300 {
		t.Fatalf("expected 300s until next fire, got %v", got["next_fire_in_seconds"])
	}
	if got["last_owner"] != "replica-a" {
		t.Fatalf("lease owner must be exposed, got %v", got["last_owner"])
	}
	if got["due_now"] != false {
		t.Fatalf("a schedule 5 minutes out must not report due_now")
	}
	if got["next_fire_key"] == "" || got["next_fire_key"] == nil {
		t.Fatalf("next_fire_key must be exposed so operators can correlate leases")
	}

	// With no state row the server still computes it rather than returning nothing.
	computed := scheduleStatusInfo(sched, scheduleState{}, now)
	if computed["next_fire_source"] != "computed" {
		t.Fatalf("expected next_fire_source=computed, got %v", computed["next_fire_source"])
	}
	if computed["next_fire_at"] != now.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("computed next fire should be now+15m, got %v", computed["next_fire_at"])
	}
	if computed["state_row"] != false {
		t.Fatalf("state_row must report false when no state row exists")
	}

	// A schedule already past its next fire reports due.
	overdue := scheduleStatusInfo(sched, scheduleState{NextFireAt: now.Add(-time.Minute)}, now)
	if overdue["due_now"] != true {
		t.Fatalf("an overdue schedule must report due_now")
	}
}
