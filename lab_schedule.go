package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Scenario scheduler (interval / daily), leased so replicas cannot double-fire ---
//
// Moved out of lab_extras.go unchanged in behaviour, then extended with:
//
//   - a lease per scheduled occurrence, so exactly one replica fires it;
//   - scheduling state in its own table, so recording a fire no longer rewrites
//     the whole scenario row (which discarded concurrent scenario edits);
//   - an auditable fire history.
//
// The lease is built on insert-then-arbitrate, because ClickHouse has no
// compare-and-set: every contender appends its claim, reads the claim set back,
// and defers to the totally-ordered winner. Every contender therefore elects the
// same owner without any replica talking to another.

const (
	defaultScheduleLeaseTTL = 5 * time.Minute
	maxScheduleLeaseClaims  = 200
	// defaultScheduleClaimSettleMS must comfortably exceed the time between two
	// replicas noticing the same due occurrence and their claim inserts landing.
	// Two seconds against a 15s minimum tick leaves a wide margin.
	defaultScheduleClaimSettleMS = 2000
)

var (
	schedulerOnce     sync.Once
	scheduleOwnerOnce sync.Once
	scheduleOwnerID   string
	scheduleRoleName  = "opl-api"
)

// setScheduleRole labels this process in lease and history rows ("opl-api" or
// "opl-orchestrator"), so the audit trail says which service won a lease.
func setScheduleRole(role string) {
	if r := strings.TrimSpace(role); r != "" {
		scheduleRoleName = r
	}
}

// scheduleOwner is this process's lease identity: stable for the process
// lifetime, distinct across replicas on the same host, and usable as a
// tie-break in the lease ordering. OPL_SCHEDULER_OWNER pins it for tests and
// for deployments that want a human-readable owner.
func scheduleOwner() string {
	scheduleOwnerOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("OPL_SCHEDULER_OWNER")); v != "" {
			scheduleOwnerID = v
			return
		}
		host, err := os.Hostname()
		if err != nil || strings.TrimSpace(host) == "" {
			host = "unknown"
		}
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			scheduleOwnerID = fmt.Sprintf("%s/%s/%d", scheduleRoleName, host, os.Getpid())
			return
		}
		scheduleOwnerID = fmt.Sprintf("%s/%s/%d-%s", scheduleRoleName, host, os.Getpid(), hex.EncodeToString(buf))
	})
	return scheduleOwnerID
}

func scheduleLeaseTTL() time.Duration {
	if v := atoiDefault(envOr("OPL_SCHEDULER_LEASE_SEC", ""), 0); v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultScheduleLeaseTTL
}

// scheduleClaimSettle is the arbitration settle window. Setting it to zero
// disables the wait and, with it, the single-fire guarantee under contention —
// only do that on a deployment that runs exactly one scheduler.
func scheduleClaimSettle() time.Duration {
	ms := atoiDefault(envOr("OPL_SCHEDULER_LEASE_SETTLE_MS", ""), defaultScheduleClaimSettleMS)
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms) * time.Millisecond
}

// --- Lease claims ---

// scheduleClaim is one row of load_schedule_leases: one contender's bid for one
// scheduled occurrence.
type scheduleClaim struct {
	ScenarioID string
	FireKey    string
	Owner      string
	Org        string
	Proj       string
	ClaimedAt  time.Time
	ExpiresAt  time.Time
	Generation int
	Released   bool
	RunID      string
}

// scheduleLeaseStore is the persistence seam for lease claims.
//
// The contract a real implementation must satisfy: once InsertClaim returns
// nil, that claim is visible to every subsequent ListClaims from any replica.
// The ClickHouse implementation gets this from a synchronous (non-async) INSERT
// followed by a plain SELECT; tests substitute an in-memory append-only store
// with the same visibility contract.
type scheduleLeaseStore interface {
	InsertClaim(c scheduleClaim) error
	ListClaims(scenarioID, fireKey string) ([]scheduleClaim, error)
}

// scheduleClaimRequest is one attempt to take an occurrence's lease.
type scheduleClaimRequest struct {
	ScenarioID string
	FireKey    string
	Owner      string
	Org        string
	Proj       string
	Now        time.Time
	TTL        time.Duration
	// Settle is how long to wait after appending this claim before arbitrating.
	// It is the protocol's one timing assumption — see claimScheduleFire.
	Settle time.Duration
}

// scheduleLeaseResult reports who owns the occurrence and whether this caller
// may fire it. Exactly one caller ever sees Acquired true for a given fire key.
type scheduleLeaseResult struct {
	Owner      string
	Acquired   bool
	FireKey    string
	Generation int
	TakenOver  bool
	ExpiresAt  time.Time
	Contenders int
	Reason     string
}

// scheduleClaimLess is the total order behind lease arbitration: earliest claim
// wins, and the lexicographically lowest owner breaks a same-timestamp tie.
// Claim timestamps are truncated to the millisecond that ClickHouse stores, so
// ties are common and the owner tie-break is load-bearing, not decorative.
func scheduleClaimLess(a, b scheduleClaim) bool {
	if !a.ClaimedAt.Equal(b.ClaimedAt) {
		return a.ClaimedAt.Before(b.ClaimedAt)
	}
	return a.Owner < b.Owner
}

// arbitrateScheduleLease elects the single owner of one scheduled occurrence
// from the full claim set.
//
// Eligibility, in order: a claim from an owner that explicitly released is out
// (that owner declined the occurrence), and a claim past its expiry is out
// (that owner died mid-run, so a later generation may take over). Among the
// remaining live claims the order is total, so every replica reading the same
// claim set elects the same owner independently.
func arbitrateScheduleLease(claims []scheduleClaim, now time.Time) (scheduleClaim, bool) {
	released := make(map[string]bool, len(claims))
	for _, c := range claims {
		if c.Released {
			released[c.Owner] = true
		}
	}
	var best scheduleClaim
	found := false
	for _, c := range claims {
		if c.Released || released[c.Owner] {
			continue
		}
		if !now.Before(c.ExpiresAt) {
			continue
		}
		if !found || scheduleClaimLess(c, best) {
			best, found = c, true
		}
	}
	return best, found
}

// scheduleLeaseGeneration reports the generation a fresh claim should carry.
// A generation above zero means at least one earlier lease on this occurrence
// expired without releasing, i.e. this claim is a takeover from a dead owner.
// Generation is audit metadata only — it never participates in the ordering,
// so two simultaneous takeovers still resolve to one winner.
func scheduleLeaseGeneration(claims []scheduleClaim, now time.Time) (generation int, takeover bool) {
	released := make(map[string]bool, len(claims))
	for _, c := range claims {
		if c.Released {
			released[c.Owner] = true
		}
	}
	for _, c := range claims {
		if c.Generation > generation {
			generation = c.Generation
		}
		if !released[c.Owner] && !c.Released && !now.Before(c.ExpiresAt) {
			takeover = true
		}
	}
	if takeover {
		return generation + 1, true
	}
	return generation, false
}

// claimScheduleFire takes the lease for one scheduled occurrence.
//
// Insert-settle-arbitrate. ClickHouse has no compare-and-set, so the protocol is:
// append this owner's claim, wait out a settle window, read the whole claim set
// back, and accept the verdict of the total order (earliest claim, lowest owner
// as tie-break).
//
// The settle window is load-bearing and is the protocol's only assumption. Without
// it the arbitration is unsafe in a way that is easy to miss: two contenders that
// stamp the same millisecond can each read a set in which they are the minimum —
// the first because the second's claim is not yet inserted, the second because it
// sorts lower on owner — and both fire. Settling first collapses that window:
//
//   - contenders whose claims land within Settle of each other all read the full
//     claim set, so they all elect the same owner and exactly one fires;
//   - a contender whose claim lands more than Settle later is strictly ordered
//     after the incumbent, so it loses on timestamp rather than on tie-break.
//
// What this buys is single-fire under bounded insert skew, not a linearizable
// lock. If two replicas' claims for one occurrence were separated by more than
// Settle *and* stamped the same millisecond by skewed clocks, both could fire.
func claimScheduleFire(store scheduleLeaseStore, req scheduleClaimRequest) (scheduleLeaseResult, error) {
	res := scheduleLeaseResult{FireKey: req.FireKey}
	if store == nil {
		return res, fmt.Errorf("no lease store configured")
	}
	// ClickHouse stores claimed_at as DateTime64(3); truncate here so the
	// in-process comparison uses exactly the precision that survives a round
	// trip, and the owner tie-break is exercised rather than bypassed.
	now := req.Now.UTC().Truncate(time.Millisecond)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultScheduleLeaseTTL
	}

	existing, err := store.ListClaims(req.ScenarioID, req.FireKey)
	if err != nil {
		return res, err
	}
	// Early-out: an occurrence already under a live lease needs no extra claim
	// row. This only saves a write — correctness comes from the insert-then-
	// arbitrate below, which still runs when contenders pass this check
	// simultaneously and each sees an empty claim set.
	if cur, ok := arbitrateScheduleLease(existing, now); ok {
		res.Owner, res.Contenders = cur.Owner, len(existing)
		res.ExpiresAt, res.Generation = cur.ExpiresAt, cur.Generation
		res.TakenOver = cur.Generation > 0
		res.Acquired = cur.Owner == req.Owner
		if res.Acquired {
			res.Reason = "lease already held by this owner"
		} else {
			res.Reason = "lease held by " + cur.Owner + " until " + cur.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return res, nil
	}

	gen, _ := scheduleLeaseGeneration(existing, now)
	mine := scheduleClaim{
		ScenarioID: req.ScenarioID, FireKey: req.FireKey, Owner: req.Owner,
		Org: req.Org, Proj: req.Proj,
		ClaimedAt: now, ExpiresAt: now.Add(ttl), Generation: gen,
	}
	if err := store.InsertClaim(mine); err != nil {
		return res, err
	}
	// Settle before arbitrating, so every contender for this occurrence reads the
	// same claim set rather than a prefix of it that makes each of them look like
	// the winner.
	if req.Settle > 0 {
		time.Sleep(req.Settle)
	}
	all, err := store.ListClaims(req.ScenarioID, req.FireKey)
	if err != nil {
		return res, err
	}
	winner, ok := arbitrateScheduleLease(all, now)
	if !ok {
		// Our own claim is live at now, so an empty election means the store
		// lost the insert. Stand down rather than fire on a lease we do not hold.
		res.Reason = "claim not visible after insert — standing down"
		return res, nil
	}
	res.Owner, res.Contenders = winner.Owner, len(all)
	res.ExpiresAt, res.Generation = winner.ExpiresAt, winner.Generation
	res.TakenOver = winner.Generation > 0
	res.Acquired = winner.Owner == req.Owner
	if res.Acquired {
		res.Reason = "lease acquired"
		if res.TakenOver {
			res.Reason = fmt.Sprintf("lease taken over at generation %d (previous owner expired)", winner.Generation)
		}
	} else {
		res.Reason = "lost arbitration to " + winner.Owner
	}
	return res, nil
}

// releaseScheduleLease records that this owner declined the occurrence without
// dispatching it, so another replica (or a later tick) may retry it now instead
// of waiting out the lease TTL. It is never used after a successful dispatch —
// a fired occurrence keeps its lease until expiry, and the advancing next fire
// time gives the following occurrence a different key.
func releaseScheduleLease(store scheduleLeaseStore, req scheduleClaimRequest, detail string) error {
	if store == nil {
		return fmt.Errorf("no lease store configured")
	}
	now := req.Now.UTC().Truncate(time.Millisecond)
	return store.InsertClaim(scheduleClaim{
		ScenarioID: req.ScenarioID, FireKey: req.FireKey, Owner: req.Owner,
		Org: req.Org, Proj: req.Proj,
		ClaimedAt: now, ExpiresAt: now, Released: true, RunID: truncateStr(detail, 200),
	})
}

// --- ClickHouse lease store ---

type chScheduleLeaseStore struct{}

// InsertClaim writes one claim synchronously. It must not be the async path:
// the arbitration SELECT that follows has to observe this row.
func (chScheduleLeaseStore) InsertClaim(c scheduleClaim) error {
	if writer == nil {
		return fmt.Errorf("clickhouse writer not configured")
	}
	released := 0
	if c.Released {
		released = 1
	}
	row, err := json.Marshal(map[string]interface{}{
		"scenario_id":     c.ScenarioID,
		"fire_key":        c.FireKey,
		"owner":           c.Owner,
		"organization_id": c.Org,
		"project_id":      c.Proj,
		"claimed_at":      c.ClaimedAt.UTC().Format("2006-01-02 15:04:05.000"),
		"expires_at":      c.ExpiresAt.UTC().Format("2006-01-02 15:04:05.000"),
		"generation":      c.Generation,
		"released":        released,
		"run_id":          c.RunID,
	})
	if err != nil {
		return err
	}
	if !writer.insert("load_schedule_leases", append(row, '\n')) {
		return fmt.Errorf("lease claim insert failed (scenario=%s fire_key=%s)", c.ScenarioID, c.FireKey)
	}
	return nil
}

func (chScheduleLeaseStore) ListClaims(scenarioID, fireKey string) ([]scheduleClaim, error) {
	if queryClient == nil {
		return nil, fmt.Errorf("clickhouse query client not configured")
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT scenario_id, fire_key, owner, organization_id, project_id,
			toUnixTimestamp64Milli(claimed_at) AS claimed_ms,
			toUnixTimestamp64Milli(expires_at) AS expires_ms,
			generation, released, run_id
		FROM `+chTable("load_schedule_leases")+`
		WHERE scenario_id = '%s' AND fire_key = '%s'
		ORDER BY claimed_at ASC, owner ASC
		LIMIT %d`, escapeSQL(scenarioID), escapeSQL(fireKey), maxScheduleLeaseClaims))
	if err != nil {
		return nil, err
	}
	out := make([]scheduleClaim, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduleClaim{
			ScenarioID: getString(row, "scenario_id"),
			FireKey:    getString(row, "fire_key"),
			Owner:      getString(row, "owner"),
			Org:        getString(row, "organization_id"),
			Proj:       getString(row, "project_id"),
			ClaimedAt:  time.UnixMilli(int64(getFloat64(row, "claimed_ms"))).UTC(),
			ExpiresAt:  time.UnixMilli(int64(getFloat64(row, "expires_ms"))).UTC(),
			Generation: int(getFloat64(row, "generation")),
			Released:   getFloat64(row, "released") == 1,
			RunID:      getString(row, "run_id"),
		})
	}
	return out, nil
}

// scheduleLeaseStoreFor returns the production store, or nil when ClickHouse is
// not wired up (in which case scheduling stays off rather than firing blind).
func scheduleLeaseStoreFor() scheduleLeaseStore {
	if queryClient == nil || writer == nil {
		return nil
	}
	return chScheduleLeaseStore{}
}

// --- Scheduling state (moved out of load_scenarios) ---

// scheduleState is one row of load_schedule_state: the scheduling bookkeeping
// that used to live inside load_scenarios.schedule_json.
type scheduleState struct {
	ScenarioID  string
	Org         string
	Proj        string
	LastFiredAt time.Time
	NextFireAt  time.Time
	LastRunID   string
	LastFireKey string
	LastOwner   string
	FireCount   int64
	UpdatedAt   time.Time
}

func (s scheduleState) empty() bool {
	return s.LastFiredAt.IsZero() && s.NextFireAt.IsZero() && s.FireCount == 0
}

// zeroDateTime64 is how ClickHouse renders the epoch default; treat it as unset.
func scheduleTimeFromMillis(v float64) time.Time {
	ms := int64(v)
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func scheduleStateFromRow(row map[string]interface{}) scheduleState {
	return scheduleState{
		ScenarioID:  getString(row, "scenario_id"),
		Org:         getString(row, "organization_id"),
		Proj:        getString(row, "project_id"),
		LastFiredAt: scheduleTimeFromMillis(getFloat64(row, "last_fired_ms")),
		NextFireAt:  scheduleTimeFromMillis(getFloat64(row, "next_fire_ms")),
		LastRunID:   getString(row, "last_run_id"),
		LastFireKey: getString(row, "last_fire_key"),
		LastOwner:   getString(row, "last_owner"),
		FireCount:   int64(getFloat64(row, "fire_count")),
		UpdatedAt:   scheduleTimeFromMillis(getFloat64(row, "updated_ms")),
	}
}

const scheduleStateSelect = `
		SELECT scenario_id, organization_id, project_id, last_run_id, last_fire_key, last_owner, fire_count,
			toUnixTimestamp64Milli(last_fired_at) AS last_fired_ms,
			toUnixTimestamp64Milli(next_fire_at) AS next_fire_ms,
			toUnixTimestamp64Milli(updated_at) AS updated_ms
		FROM `

// loadScheduleStates reads scheduling state for every scenario in one query.
// A missing table (pre-migration deployment) is an empty map, not an error, so
// the scheduler degrades to the schedule_json fallback instead of stopping.
func loadScheduleStates() map[string]scheduleState {
	out := map[string]scheduleState{}
	if queryClient == nil {
		return out
	}
	rows, err := queryClient.Query(scheduleStateSelect + chTable("load_schedule_state") + ` FINAL LIMIT 1000`)
	if err != nil {
		return out
	}
	for _, row := range rows {
		st := scheduleStateFromRow(row)
		if st.ScenarioID != "" {
			out[st.ScenarioID] = st
		}
	}
	return out
}

func loadScheduleStateFor(scenarioID, org, proj string) scheduleState {
	if queryClient == nil || scenarioID == "" {
		return scheduleState{ScenarioID: scenarioID, Org: org, Proj: proj}
	}
	rows, err := queryClient.Query(fmt.Sprintf(scheduleStateSelect+chTable("load_schedule_state")+
		` FINAL WHERE scenario_id = '%s' LIMIT 1`, escapeSQL(scenarioID)))
	if err != nil || len(rows) == 0 {
		return scheduleState{ScenarioID: scenarioID, Org: org, Proj: proj}
	}
	return scheduleStateFromRow(rows[0])
}

// writeScheduleState upserts only the scheduling columns.
//
// This is the fix for the clobbering bug: the previous code rebuilt the entire
// load_scenarios row (name, target_url, steps_json, jmx_xml, …) from a snapshot
// read before the fire, so any scenario edit committed in between was silently
// reverted. Nothing here touches load_scenarios.
func writeScheduleState(st scheduleState) {
	if writer == nil || st.ScenarioID == "" {
		return
	}
	row := map[string]interface{}{
		"scenario_id":     st.ScenarioID,
		"organization_id": st.Org,
		"project_id":      st.Proj,
		"last_run_id":     st.LastRunID,
		"last_fire_key":   st.LastFireKey,
		"last_owner":      st.LastOwner,
		"fire_count":      st.FireCount,
		"updated_at":      time.Now().UTC().Format("2006-01-02 15:04:05.000"),
	}
	if !st.LastFiredAt.IsZero() {
		row["last_fired_at"] = st.LastFiredAt.UTC().Format("2006-01-02 15:04:05.000")
	}
	if !st.NextFireAt.IsZero() {
		row["next_fire_at"] = st.NextFireAt.UTC().Format("2006-01-02 15:04:05.000")
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return
	}
	writer.insertAsync("load_schedule_state", append(payload, '\n'))
}

// --- Fire history ---

// scheduleFireRecord is one row of load_schedule_fires.
type scheduleFireRecord struct {
	ScenarioID string
	Org        string
	Proj       string
	FireKey    string
	Owner      string
	RunID      string
	Outcome    string
	Detail     string
	Source     string
	VUs        int
	NextFireAt time.Time
	FiredAt    time.Time
}

func newScheduleFireID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("shf-%d", time.Now().UnixNano())
	}
	return "shf-" + hex.EncodeToString(buf)
}

// recordScheduleFire appends one audit row. Losing contenders are not recorded
// here — load_schedule_leases already holds every claim, so contention is
// auditable from the lease table and this stays one row per actual fire.
func recordScheduleFire(rec scheduleFireRecord) {
	if writer == nil || rec.ScenarioID == "" {
		return
	}
	firedAt := rec.FiredAt
	if firedAt.IsZero() {
		firedAt = time.Now().UTC()
	}
	row := map[string]interface{}{
		"id":              newScheduleFireID(),
		"scenario_id":     rec.ScenarioID,
		"organization_id": rec.Org,
		"project_id":      rec.Proj,
		"fire_key":        rec.FireKey,
		"owner":           rec.Owner,
		"run_id":          rec.RunID,
		"outcome":         nz(rec.Outcome, "fired"),
		"vus":             rec.VUs,
		"detail":          truncateStr(rec.Detail, 1000),
		"source":          nz(rec.Source, "scheduler"),
		"fired_at":        firedAt.UTC().Format("2006-01-02 15:04:05.000"),
	}
	if !rec.NextFireAt.IsZero() {
		row["next_fire_at"] = rec.NextFireAt.UTC().Format("2006-01-02 15:04:05.000")
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return
	}
	writer.insertAsync("load_schedule_fires", append(payload, '\n'))
}

func queryScheduleFireHistory(r *http.Request, scenarioID string, limit int) ([]map[string]interface{}, error) {
	if queryClient == nil {
		return nil, fmt.Errorf("not ready")
	}
	where := " WHERE 1=1" + tenantScopeSQL(r, queryClient, "")
	if scenarioID != "" {
		where += fmt.Sprintf(" AND scenario_id = '%s'", escapeSQL(scenarioID))
	}
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("outcome"))); v != "" {
		where += fmt.Sprintf(" AND outcome = '%s'", escapeSQL(v))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("owner")); v != "" {
		where += fmt.Sprintf(" AND owner = '%s'", escapeSQL(v))
	}
	return queryClient.Query(fmt.Sprintf(`
		SELECT id, scenario_id, fire_key, owner, run_id, outcome, vus, detail, source,
			next_fire_at, fired_at
		FROM `+chTable("load_schedule_fires")+`%s
		ORDER BY fired_at DESC
		LIMIT %d`, where, limit))
}

func scheduleHistoryLimit(r *http.Request) int {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = clampInt(n, 1, 500)
		}
	}
	return limit
}

// --- Due / next-fire arithmetic ---

// scheduleEveryMinutes reads the interval, accepting both spellings the API has
// always accepted.
func scheduleEveryMinutes(sched map[string]interface{}) int {
	every := int(getFloat64(sched, "every_minutes"))
	if every <= 0 {
		every = int(getFloat64(sched, "interval_minutes"))
	}
	return every
}

// scheduleDailyAt reads the daily HH:MM (UTC), accepting both spellings.
func scheduleDailyAt(sched map[string]interface{}) string {
	return strings.TrimSpace(firstNonEmpty(getString(sched, "daily_at"), getString(sched, "cron_time")))
}

func parseDailyAt(daily string) (hh, mm int, ok bool) {
	parts := strings.Split(daily, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	hh, mm = atoiDefault(parts[0], -1), atoiDefault(parts[1], -1)
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// scheduleEnabled reports whether the schedule is switched on, accepting the
// boolean and the numeric-1 encodings both already in the field.
func scheduleEnabled(sched map[string]interface{}) bool {
	if enabled, _ := sched["enabled"].(bool); enabled {
		return true
	}
	return getFloat64(sched, "enabled") == 1
}

// scheduleIsDue is the schedule_json-only due check, unchanged from the version
// that lived in lab_extras.go. It remains the fallback for deployments whose
// scheduling state is still embedded in schedule_json.
func scheduleIsDue(sched map[string]interface{}, now time.Time) bool {
	if next := strings.TrimSpace(getString(sched, "next_fire_at")); next != "" {
		if t, err := time.Parse(time.RFC3339, next); err == nil {
			return !now.Before(t)
		}
	}
	every := scheduleEveryMinutes(sched)
	last := strings.TrimSpace(getString(sched, "last_fired_at"))
	if every > 0 {
		if last == "" {
			return true
		}
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			return now.Sub(t) >= time.Duration(every)*time.Minute
		}
		return true
	}
	daily := scheduleDailyAt(sched)
	if daily == "" {
		return false
	}
	hh, mm, ok := parseDailyAt(daily)
	if !ok {
		return false
	}
	if now.Hour() != hh || now.Minute() != mm {
		return false
	}
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && t.UTC().Format("2006-01-02") == now.Format("2006-01-02") {
			return false
		}
	}
	return true
}

// scheduleDueWithState is the due check once scheduling state has its own row.
// The state row is authoritative when present; an empty state row falls back to
// scheduleIsDue so a scenario written before the split still fires.
func scheduleDueWithState(sched map[string]interface{}, st scheduleState, now time.Time) bool {
	if !st.NextFireAt.IsZero() {
		return !now.Before(st.NextFireAt)
	}
	if !st.LastFiredAt.IsZero() {
		if every := scheduleEveryMinutes(sched); every > 0 {
			return now.Sub(st.LastFiredAt) >= time.Duration(every)*time.Minute
		}
		daily := scheduleDailyAt(sched)
		if daily == "" {
			return false
		}
		hh, mm, ok := parseDailyAt(daily)
		if !ok {
			return false
		}
		if now.UTC().Hour() != hh || now.UTC().Minute() != mm {
			return false
		}
		return st.LastFiredAt.UTC().Format("2006-01-02") != now.UTC().Format("2006-01-02")
	}
	return scheduleIsDue(sched, now)
}

// nextFireFromSchedule computes the next occurrence, unchanged from the version
// that lived in lab_extras.go.
func nextFireFromSchedule(sched map[string]interface{}, from time.Time) time.Time {
	if every := scheduleEveryMinutes(sched); every > 0 {
		return from.Add(time.Duration(every) * time.Minute)
	}
	if daily := scheduleDailyAt(sched); daily != "" {
		if hh, mm, ok := parseDailyAt(daily); ok {
			next := time.Date(from.Year(), from.Month(), from.Day(), hh, mm, 0, 0, time.UTC)
			if !next.After(from) {
				next = next.Add(24 * time.Hour)
			}
			return next
		}
	}
	return from.Add(24 * time.Hour)
}

// scheduleFireKey names one scheduled occurrence.
//
// Every replica must derive the same key for the same occurrence, or they would
// contend on different leases and both fire. The key is therefore built only
// from the persisted next fire time or the schedule definition — never from
// wall-clock nanoseconds, which would never collide and so never arbitrate.
func scheduleFireKey(sched map[string]interface{}, st scheduleState, due time.Time) string {
	if !st.NextFireAt.IsZero() {
		return "at:" + st.NextFireAt.UTC().Truncate(time.Second).Format("20060102T150405Z")
	}
	if next := strings.TrimSpace(getString(sched, "next_fire_at")); next != "" {
		if t, err := time.Parse(time.RFC3339, next); err == nil {
			return "at:" + t.UTC().Truncate(time.Second).Format("20060102T150405Z")
		}
	}
	if every := scheduleEveryMinutes(sched); every > 0 {
		return fmt.Sprintf("iv:%d:%d", every, due.UTC().Unix()/int64(every*60))
	}
	if daily := scheduleDailyAt(sched); daily != "" {
		return "day:" + due.UTC().Format("20060102") + ":" + strings.ReplaceAll(daily, ":", "")
	}
	return "min:" + due.UTC().Format("200601021504")
}

// --- Scheduler loop ---

func startPerfScheduler() {
	schedulerOnce.Do(func() {
		if envFlagOn("OPA_PERF_SCHEDULER_DISABLE") {
			log.Printf("[scheduler] disabled via OPA_PERF_SCHEDULER_DISABLE")
			return
		}
		interval := time.Duration(atoiDefault(envOr("OPA_PERF_SCHEDULER_TICK_SEC", "60"), 60)) * time.Second
		if interval < 15*time.Second {
			interval = 15 * time.Second
		}
		go func() {
			log.Printf("[scheduler] owner=%s tick every %s — leased, so extra replicas stand down instead of double-firing",
				scheduleOwner(), interval)
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				fireDueSchedules()
			}
		}()
	})
}

// scheduleTickResult summarises one tick for logging and for the orchestrator's
// state endpoint.
type scheduleTickResult struct {
	Considered int                      `json:"considered"`
	Due        int                      `json:"due"`
	Fired      int                      `json:"fired"`
	LostLease  int                      `json:"lost_lease"`
	Errors     int                      `json:"errors"`
	Owner      string                   `json:"owner"`
	Fires      []map[string]interface{} `json:"fires,omitempty"`
}

func fireDueSchedules() scheduleTickResult {
	out := scheduleTickResult{Owner: scheduleOwner()}
	if queryClient == nil || writer == nil {
		return out
	}
	rows, err := queryClient.Query(`
		SELECT id, name, organization_id, project_id, vus, duration_seconds, schedule_json, target_url,
			method, headers_json, body, thresholds_json, steps_json, datasets_json, sla_json, jmx_xml
		FROM ` + chTable("load_scenarios") + ` FINAL
		WHERE coalesce(archived, 0) = 0
		ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		out.Errors++
		return out
	}
	states := loadScheduleStates()
	store := scheduleLeaseStoreFor()
	ttl := scheduleLeaseTTL()
	settle := scheduleClaimSettle()
	owner := scheduleOwner()
	now := time.Now().UTC()
	for _, row := range rows {
		schedRaw := getString(row, "schedule_json")
		if schedRaw == "" || schedRaw == "{}" {
			continue
		}
		var sched map[string]interface{}
		if json.Unmarshal([]byte(schedRaw), &sched) != nil {
			continue
		}
		if !scheduleEnabled(sched) {
			continue
		}
		out.Considered++
		scnID := getString(row, "id")
		st := states[scnID]
		if !scheduleDueWithState(sched, st, now) {
			continue
		}
		out.Due++
		org := nz(getString(row, "organization_id"), defaultOrgID)
		proj := nz(getString(row, "project_id"), defaultProjectID)
		fireKey := scheduleFireKey(sched, st, now)
		claimReq := scheduleClaimRequest{
			ScenarioID: scnID, FireKey: fireKey, Owner: owner,
			Org: org, Proj: proj, Now: now, TTL: ttl, Settle: settle,
		}
		lease, err := claimScheduleFire(store, claimReq)
		if err != nil {
			out.Errors++
			log.Printf("[scheduler] lease error scenario=%s fire_key=%s: %v — not firing", scnID, fireKey, err)
			continue
		}
		if !lease.Acquired {
			out.LostLease++
			log.Printf("[scheduler] scenario=%s fire_key=%s owned by %s — standing down (%s)",
				scnID, fireKey, lease.Owner, lease.Reason)
			continue
		}
		fired := fireScenarioNow(row, sched, st, claimReq, lease)
		if fired != nil {
			out.Fired++
			out.Fires = append(out.Fires, fired)
		}
	}
	return out
}

// fireScenarioNow dispatches one owned occurrence and records it. The caller
// must already hold the lease for claimReq.FireKey.
func fireScenarioNow(row, sched map[string]interface{}, st scheduleState, claimReq scheduleClaimRequest, lease scheduleLeaseResult) map[string]interface{} {
	scnID, org, proj := claimReq.ScenarioID, claimReq.Org, claimReq.Proj
	now := claimReq.Now
	store := scheduleLeaseStoreFor()

	vus := int(getFloat64(row, "vus"))
	if v := int(getFloat64(sched, "vus")); v > 0 {
		vus = v
	}
	workers := int(getFloat64(sched, "workers"))
	policy, _ := sched["policy"].(string)
	profile, _ := sched["profile"].(string)
	dispatch := true
	if v, ok := sched["dispatch"].(bool); ok {
		dispatch = v
	}
	if curve := parseCurveFromSchedule(sched); len(curve) > 0 {
		if peak, _, _ := applyLoadCurveToSchedule(curve, sched); peak > 0 {
			vus = peak
		}
	}
	_, resolvedSched, _ := resolveLoadPolicy(policy, profile, sched)
	vus = clampPerfVUs(vus)
	// The run id is derived from the fire key, not from the wall clock, so the
	// lease owner and the run it produced stay tied together in the audit trail.
	runID := loadID("run", org, proj, scnID, "sched-"+claimReq.FireKey)
	ts := now.Format("2006-01-02 15:04:05.000")
	payload, err := json.Marshal(map[string]interface{}{
		"id": runID, "organization_id": org, "project_id": proj,
		"scenario_id": scnID, "status": "running", "vus": vus,
		"started_at": ts, "finished_at": ts, "summary_json": `{"scheduled":true}`, "error": "",
	})
	if err != nil {
		_ = releaseScheduleLease(store, claimReq, "run row marshal failed")
		return nil
	}
	writer.insertAsync("load_runs", append(payload, '\n'))

	outcome, detail := "fired", lease.Reason
	if dispatch {
		dispatchInfo := dispatchJMeterRunScaled(scnID, runID, vus, workers, org, proj)
		runStatus, runErr := initialLoadRunStatus(true, dispatchInfo)
		if names := containerNamesFromAny(dispatchInfo["containers"]); len(names) > 0 {
			registerRunContainers(runID, names, getString(dispatchInfo, "mode"), getString(dispatchInfo, "image"), int(getFloat64(dispatchInfo, "workers")))
		}
		if runStatus != "running" {
			outcome, detail = "dispatch_failed", runErr
			sum, _ := json.Marshal(map[string]interface{}{"scheduled": true, "dispatch_error": runErr, "policy": resolvedSched["policy"]})
			fix, _ := json.Marshal(map[string]interface{}{
				"id": runID, "organization_id": org, "project_id": proj,
				"scenario_id": scnID, "status": runStatus, "vus": vus,
				"started_at": ts, "finished_at": ts, "summary_json": string(sum), "error": runErr,
			})
			writer.insertAsync("load_runs", append(fix, '\n'))
			if runStatusTerminal(runStatus) {
				notifyRunTerminal(runNotifyEvent{
					RunID: runID, ScenarioID: scnID, OrganizationID: org, ProjectID: proj,
					Status: runStatus, VUs: vus, Error: runErr,
					Summary:    map[string]interface{}{"scheduled": true, "dispatch_error": runErr},
					FinishedAt: ts, Source: "scheduler",
				})
			}
		}
	} else {
		outcome = "recorded_no_dispatch"
	}

	next := nextFireFromSchedule(sched, now)
	// Scheduling state only — load_scenarios is not written, so a scenario edit
	// racing this fire survives.
	writeScheduleState(scheduleState{
		ScenarioID: scnID, Org: org, Proj: proj,
		LastFiredAt: now, NextFireAt: next,
		LastRunID: runID, LastFireKey: claimReq.FireKey, LastOwner: claimReq.Owner,
		FireCount: st.FireCount + 1,
	})
	recordScheduleFire(scheduleFireRecord{
		ScenarioID: scnID, Org: org, Proj: proj,
		FireKey: claimReq.FireKey, Owner: claimReq.Owner, RunID: runID,
		Outcome: outcome, Detail: detail, Source: scheduleRoleName,
		VUs: vus, NextFireAt: next, FiredAt: now,
	})
	log.Printf("[scheduler] fired scenario=%s run=%s vus=%d fire_key=%s owner=%s outcome=%s",
		scnID, runID, vus, claimReq.FireKey, claimReq.Owner, outcome)
	return map[string]interface{}{
		"scenario_id": scnID, "run_id": runID, "vus": vus,
		"fire_key": claimReq.FireKey, "owner": claimReq.Owner, "outcome": outcome,
		"next_fire_at": next.Format(time.RFC3339),
	}
}

// bumpScenarioSchedule persists an operator's schedule *definition* edit.
//
// It still reads the scenario back and rewrites the row, because that is what
// an explicit definition edit is. What it no longer carries is scheduling state
// (last_fired_at / next_fire_at) — that lives in load_schedule_state, so the
// background fire path never takes this write path at all.
func bumpScenarioSchedule(id, org, proj string, row, sched map[string]interface{}) {
	if writer == nil {
		return
	}
	schedJSON, _ := json.Marshal(sched)
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	sc := loadScenarioMapForTenant(id, org, proj)
	if sc == nil {
		sc = row
	}
	payload, err := json.Marshal(map[string]interface{}{
		"id": id, "organization_id": org, "project_id": proj,
		"name": getString(sc, "name"), "target_url": getString(sc, "target_url"),
		"method": nz(getString(sc, "method"), "GET"),
		"vus":    int(getFloat64(sc, "vus")), "duration_seconds": int(getFloat64(sc, "duration_seconds")),
		"headers_json": nz(getString(sc, "headers_json"), "{}"), "body": getString(sc, "body"),
		"thresholds_json": nz(getString(sc, "thresholds_json"), "{}"),
		"steps_json":      nz(getString(sc, "steps_json"), "[]"), "datasets_json": nz(getString(sc, "datasets_json"), "{}"),
		"sla_json": nz(getString(sc, "sla_json"), "{}"), "schedule_json": string(schedJSON),
		"jmx_xml": getString(sc, "jmx_xml"), "archived": 0,
		"updated_at": now, "created_at": now,
	})
	if err != nil {
		return
	}
	writer.insertAsync("load_scenarios", append(payload, '\n'))
}

// --- HTTP handlers ---

// scheduleStatusInfo is the server-computed schedule view the UI reads instead
// of recomputing the next fire time in the browser.
func scheduleStatusInfo(sched map[string]interface{}, st scheduleState, now time.Time) map[string]interface{} {
	out := map[string]interface{}{
		"enabled":    scheduleEnabled(sched),
		"fire_count": st.FireCount,
		"due_now":    scheduleEnabled(sched) && scheduleDueWithState(sched, st, now),
		"state_row":  !st.empty(),
	}
	if every := scheduleEveryMinutes(sched); every > 0 {
		out["every_minutes"] = every
	}
	if daily := scheduleDailyAt(sched); daily != "" {
		out["daily_at"] = daily
	}
	next := st.NextFireAt
	if next.IsZero() {
		next = nextFireFromSchedule(sched, now)
		out["next_fire_source"] = "computed"
	} else {
		out["next_fire_source"] = "state_row"
	}
	out["next_fire_at"] = next.UTC().Format(time.RFC3339)
	out["next_fire_in_seconds"] = int(next.UTC().Sub(now).Round(time.Second).Seconds())
	if !st.LastFiredAt.IsZero() {
		out["last_fired_at"] = st.LastFiredAt.UTC().Format(time.RFC3339)
	}
	if st.LastRunID != "" {
		out["last_run_id"] = st.LastRunID
	}
	if st.LastFireKey != "" {
		out["last_fire_key"] = st.LastFireKey
	}
	if st.LastOwner != "" {
		out["last_owner"] = st.LastOwner
	}
	out["next_fire_key"] = scheduleFireKey(sched, st, now)
	return out
}

// scheduleLeaseInfo reports who currently holds the lease for the next
// occurrence, so the UI can show which replica will fire it.
func scheduleLeaseInfo(sched map[string]interface{}, st scheduleState, now time.Time) map[string]interface{} {
	info := map[string]interface{}{
		"this_owner":  scheduleOwner(),
		"ttl_seconds": int(scheduleLeaseTTL().Seconds()),
	}
	store := scheduleLeaseStoreFor()
	if store == nil {
		info["honesty"] = "Lease state unavailable — ClickHouse is not wired up in this process."
		return info
	}
	fireKey := scheduleFireKey(sched, st, now)
	info["fire_key"] = fireKey
	claims, err := store.ListClaims(st.ScenarioID, fireKey)
	if err != nil {
		info["honesty"] = "Lease table not readable on this deployment yet (no claims recorded)."
		return info
	}
	info["claims"] = len(claims)
	if winner, ok := arbitrateScheduleLease(claims, now); ok {
		info["owner"] = winner.Owner
		info["expires_at"] = winner.ExpiresAt.UTC().Format(time.RFC3339)
		info["generation"] = winner.Generation
		info["held_by_this_process"] = winner.Owner == scheduleOwner()
	} else {
		info["owner"] = ""
		info["honesty"] = "No live lease on the next occurrence — the first replica to reach it will claim it."
	}
	return info
}

const scheduleHonesty = "Scheduler ticks in opl-api and opl-orchestrator (every_minutes or daily_at HH:MM UTC). " +
	"Each occurrence is leased, so extra replicas stand down instead of double-firing; the lease is arbitrated from " +
	"append-only claims, not a compare-and-set, so it depends on a claim being visible to every reader once its " +
	"insert returns. Scheduling state lives in load_schedule_state, so a fire never rewrites the scenario row. " +
	"curve_mode=arrivals compiles open-model start segments."

// handlePerfScenarioSchedule serves GET (server-computed next fire + lease
// owner), POST/PUT (edit the schedule definition), and routes .../history.
func handlePerfScenarioSchedule(w http.ResponseWriter, r *http.Request, id string, sub string) {
	if sub == "history" {
		handlePerfScheduleHistory(w, r, id)
		return
	}
	if sub != "" {
		http.Error(w, "not found", 404)
		return
	}
	if queryClient == nil {
		http.Error(w, "not ready", 503)
		return
	}
	if r.Method == http.MethodGet {
		handlePerfScenarioScheduleGet(w, r, id)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !perfRequireAdmin(w, r) {
		return
	}
	if writer == nil {
		http.Error(w, "not ready", 503)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	var patch map[string]interface{}
	if json.Unmarshal(raw, &patch) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	sched := map[string]interface{}{}
	if s := getString(sc, "schedule_json"); s != "" && s != "{}" {
		_ = json.Unmarshal([]byte(s), &sched)
	}
	for k, v := range patch {
		sched[k] = v
	}
	if curve := parseCurveFromSchedule(sched); len(curve) > 0 {
		_, _, _ = applyLoadCurveToSchedule(curve, sched)
	}
	now := time.Now().UTC()
	st := loadScheduleStateFor(id, org, proj)
	st.ScenarioID, st.Org, st.Proj = id, org, proj
	// The next fire time is now server state, not a field the caller round-trips
	// through schedule_json.
	if scheduleEnabled(sched) && st.NextFireAt.IsZero() {
		st.NextFireAt = nextFireFromSchedule(sched, now)
		writeScheduleState(st)
	}
	bumpScenarioSchedule(id, org, proj, sc, sched)
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "schedule": sched,
		"status":  scheduleStatusInfo(sched, st, now),
		"lease":   scheduleLeaseInfo(sched, st, now),
		"honesty": scheduleHonesty,
	})
}

func handlePerfScenarioScheduleGet(w http.ResponseWriter, r *http.Request, id string) {
	sc := loadScenarioMapReq(r, id)
	if sc == nil {
		http.Error(w, "not found", 404)
		return
	}
	ctx, _ := ExtractTenantContext(r, queryClient)
	org, proj := ctx.WriteTenant()
	sched := map[string]interface{}{}
	if s := getString(sc, "schedule_json"); s != "" && s != "{}" {
		_ = json.Unmarshal([]byte(s), &sched)
	}
	now := time.Now().UTC()
	st := loadScheduleStateFor(id, org, proj)
	st.ScenarioID = id
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": id, "schedule": sched,
		"status":  scheduleStatusInfo(sched, st, now),
		"lease":   scheduleLeaseInfo(sched, st, now),
		"honesty": scheduleHonesty,
	})
}

// handlePerfScheduleHistory serves GET /api/perf/scenarios/{id}/schedule/history.
func handlePerfScheduleHistory(w http.ResponseWriter, r *http.Request, scenarioID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	rows, err := queryScheduleFireHistory(r, scenarioID, scheduleHistoryLimit(r))
	if err != nil {
		if err.Error() == "not ready" {
			http.Error(w, "not ready", 503)
			return
		}
		// A missing table on a pre-migration deployment is an empty history, not a 500.
		writeJSON(w, map[string]interface{}{
			"ok": true, "scenario_id": scenarioID, "fires": []interface{}{}, "count": 0,
			"honesty": "No schedule fire history available yet (table not initialised on this deployment). " + scheduleHonesty,
		})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "scenario_id": scenarioID, "fires": rows, "count": len(rows),
		"honesty": "One row per occurrence this deployment actually fired, with the lease owner that won it. " +
			"Replicas that lost arbitration are not listed here — every claim, winning or losing, is in load_schedule_leases.",
	})
}

// handlePerfSchedules serves GET /api/perf/schedules: every enabled schedule
// with its server-computed next fire time, so the UI never guesses.
func handlePerfSchedules(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/perf/schedules"), "/")
	if sub == "history" {
		handlePerfScheduleHistory(w, r, strings.TrimSpace(r.URL.Query().Get("scenario_id")))
		return
	}
	if sub != "" {
		http.Error(w, "not found", 404)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if queryClient == nil {
		writeJSON(w, map[string]interface{}{"ok": true, "schedules": []interface{}{}, "count": 0, "honesty": scheduleHonesty})
		return
	}
	rows, err := queryClient.Query(fmt.Sprintf(`
		SELECT id, name, organization_id, project_id, schedule_json
		FROM `+chTable("load_scenarios")+` FINAL WHERE 1=1%s%s
		ORDER BY updated_at DESC LIMIT 200`, tenantScopeSQL(r, queryClient, ""), scenarioArchivedAnd()))
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": true, "schedules": []interface{}{}, "count": 0, "honesty": scheduleHonesty})
		return
	}
	states := loadScheduleStates()
	now := time.Now().UTC()
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		raw := getString(row, "schedule_json")
		if raw == "" || raw == "{}" {
			continue
		}
		var sched map[string]interface{}
		if json.Unmarshal([]byte(raw), &sched) != nil {
			continue
		}
		id := getString(row, "id")
		st := states[id]
		st.ScenarioID = id
		entry := map[string]interface{}{
			"scenario_id": id, "name": getString(row, "name"),
			"status": scheduleStatusInfo(sched, st, now),
		}
		out = append(out, entry)
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "schedules": out, "count": len(out),
		"this_owner": scheduleOwner(),
		"honesty":    scheduleHonesty,
	})
}
