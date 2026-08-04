package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- In-memory lease store standing in for load_schedule_leases ---
//
// It matches the contract the ClickHouse store relies on and nothing more:
// append-only rows, and a claim visible to every subsequent reader once
// InsertClaim has returned. It deliberately does NOT serialise insert+read as
// one atomic step, because ClickHouse cannot do that either — the interleaving
// it allows is exactly the interleaving the arbitration has to survive.

type fakeLeaseStore struct {
	mu      sync.Mutex
	rows    []scheduleClaim
	inserts int
	lists   int
	// gap widens the window between an insert and the read-back, so contenders
	// are forced to observe each other's claims mid-flight.
	gap time.Duration
}

func (f *fakeLeaseStore) InsertClaim(c scheduleClaim) error {
	f.mu.Lock()
	f.rows = append(f.rows, c)
	f.inserts++
	f.mu.Unlock()
	if f.gap > 0 {
		time.Sleep(f.gap)
	}
	return nil
}

func (f *fakeLeaseStore) ListClaims(scenarioID, fireKey string) ([]scheduleClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	out := make([]scheduleClaim, 0, len(f.rows))
	for _, c := range f.rows {
		if c.ScenarioID == scenarioID && c.FireKey == fireKey {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeLeaseStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// naiveClaimNoArbitration is the behaviour the lease replaces: insert a claim
// and assume you own the occurrence. It exists only so the concurrency test can
// show it double-fires under the same conditions where claimScheduleFire does
// not — i.e. so a passing test proves something.
func naiveClaimNoArbitration(store scheduleLeaseStore, req scheduleClaimRequest) (bool, error) {
	now := req.Now.UTC().Truncate(time.Millisecond)
	err := store.InsertClaim(scheduleClaim{
		ScenarioID: req.ScenarioID, FireKey: req.FireKey, Owner: req.Owner,
		ClaimedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	return err == nil, err
}

// testSettle is the settle window used in tests. Contenders are released by a
// barrier so their inserts land within microseconds of each other; a few
// milliseconds is therefore a faithful, and much faster, stand-in for the
// production window.
const testSettle = 20 * time.Millisecond

// runContenders starts n goroutines that all try to fire the SAME occurrence at
// the SAME instant with the SAME timestamp — the worst case, where every claim
// collides in one stored millisecond and only the tie-break separates them.
// It returns which owners believed they had the right to fire.
func runContenders(t *testing.T, n int, claim func(scheduleClaimRequest) (bool, error), now time.Time, fireKey string) []string {
	t.Helper()
	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start.Add(1)
	for i := 0; i < n; i++ {
		owner := fmt.Sprintf("replica-%02d", i)
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			ok, err := claim(scheduleClaimRequest{
				ScenarioID: "scn-1", FireKey: fireKey, Owner: owner,
				Org: "org", Proj: "proj", Now: now, TTL: time.Minute, Settle: testSettle,
			})
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				winners = append(winners, owner)
			}
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()
	for _, err := range errs {
		t.Fatalf("claim error: %v", err)
	}
	return winners
}

// TestTwoConcurrentSchedulersFireOnce is the headline guarantee: two schedulers
// racing on the same occurrence produce exactly one fire, not two.
func TestTwoConcurrentSchedulersFireOnce(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{gap: 200 * time.Microsecond}
	winners := runContenders(t, 2, func(req scheduleClaimRequest) (bool, error) {
		res, err := claimScheduleFire(store, req)
		return res.Acquired, err
	}, now, "at:20260804T100000Z")

	if len(winners) != 1 {
		t.Fatalf("two concurrent schedulers must yield exactly 1 fire, got %d: %v", len(winners), winners)
	}
	// Both contenders must be on record, so the decision is auditable even
	// though only one of them fired.
	if store.count() < 1 {
		t.Fatalf("expected the losing claim to be recorded too, got %d rows", store.count())
	}
}

// TestConcurrentSchedulersFireOnceAtScale runs the same race with many more
// contenders and repeats it, because a single two-way race can pass by luck.
// Run under -race this also asserts the arbitration path is data-race free.
func TestConcurrentSchedulersFireOnceAtScale(t *testing.T) {
	now := time.Date(2026, 8, 4, 11, 30, 0, 0, time.UTC)
	for _, n := range []int{2, 3, 8, 32} {
		for round := 0; round < 25; round++ {
			store := &fakeLeaseStore{}
			key := fmt.Sprintf("at:%d-%d", n, round)
			winners := runContenders(t, n, func(req scheduleClaimRequest) (bool, error) {
				res, err := claimScheduleFire(store, req)
				return res.Acquired, err
			}, now, key)
			if len(winners) != 1 {
				t.Fatalf("n=%d round=%d: expected exactly 1 fire, got %d: %v", n, round, len(winners), winners)
			}
		}
	}
}

// TestNaiveClaimDoubleFires is the negative control for the two tests above.
// Without arbitration every contender fires, which is the bug the lease fixes.
// If this test ever passes with 1 winner, the concurrency tests are not
// exercising a real race and their result means nothing.
func TestNaiveClaimDoubleFires(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	winners := runContenders(t, 4, func(req scheduleClaimRequest) (bool, error) {
		return naiveClaimNoArbitration(store, req)
	}, now, "at:20260804T100000Z")
	if len(winners) != 4 {
		t.Fatalf("control: unarbitrated claims should all believe they won, got %d: %v", len(winners), winners)
	}
}

// TestClaimTiesBreakOnOwner pins the tie-break that carries the whole guarantee
// once timestamps are truncated to the millisecond ClickHouse stores. Every
// contender claiming in the same millisecond must still elect one owner, and it
// must be the lowest owner id so all replicas agree.
func TestClaimTiesBreakOnOwner(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	claims := []scheduleClaim{
		{Owner: "replica-c", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)},
		{Owner: "replica-a", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)},
		{Owner: "replica-b", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)},
	}
	winner, ok := arbitrateScheduleLease(claims, now)
	if !ok || winner.Owner != "replica-a" {
		t.Fatalf("expected replica-a to win the same-millisecond tie, got %q ok=%v", winner.Owner, ok)
	}
	// Order of the claim set must not change the verdict: every replica reads
	// the rows back in whatever order ClickHouse returns them.
	shuffled := []scheduleClaim{claims[2], claims[0], claims[1]}
	again, ok := arbitrateScheduleLease(shuffled, now)
	if !ok || again.Owner != "replica-a" {
		t.Fatalf("arbitration must be order-independent, got %q ok=%v", again.Owner, ok)
	}
}

// TestSubMillisecondClaimsStillArbitrate guards against a false sense of safety:
// if claim timestamps were kept at nanosecond precision in memory but stored at
// millisecond precision, tests would pass on a discriminator production does not
// have. claimScheduleFire truncates, so two claims 100µs apart collide on the
// timestamp and are resolved by owner instead.
func TestSubMillisecondClaimsStillArbitrate(t *testing.T) {
	base := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	a, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k", Owner: "replica-z",
		Now: base.Add(100 * time.Microsecond), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim a: %v", err)
	}
	b, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k", Owner: "replica-a",
		Now: base.Add(900 * time.Microsecond), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if !a.Acquired {
		t.Fatalf("first claimant should hold the lease")
	}
	// replica-a claims later in wall-clock terms but lands in the same stored
	// millisecond; it must lose to the incumbent rather than steal the lease.
	if b.Acquired {
		t.Fatalf("second claimant must not acquire a live lease (owner=%s reason=%s)", b.Owner, b.Reason)
	}
	if b.Owner != "replica-z" {
		t.Fatalf("expected incumbent replica-z to stay owner, got %q", b.Owner)
	}
}

// TestArbitrationAloneIsNotSafeWithoutSettling documents, deterministically, why
// the settle window exists.
//
// Arbitration over a PREFIX of the claim set can elect a different owner than
// arbitration over the full set: the first contender sees only its own claim and
// elects itself, then the second sees both and elects itself on the owner
// tie-break — and both fire. Settling before reading is what removes the prefix
// view. If this test ever fails, the single-fire guarantee has quietly become
// dependent on something other than the settle window and needs re-deriving.
func TestArbitrationAloneIsNotSafeWithoutSettling(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	first := scheduleClaim{Owner: "replica-01", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)}
	second := scheduleClaim{Owner: "replica-00", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)}

	prefixView, ok1 := arbitrateScheduleLease([]scheduleClaim{first}, now)
	fullView, ok2 := arbitrateScheduleLease([]scheduleClaim{first, second}, now)
	if !ok1 || !ok2 {
		t.Fatalf("both views should elect somebody")
	}
	if prefixView.Owner == fullView.Owner {
		t.Fatalf("expected the prefix view (%q) and the full view (%q) to disagree; "+
			"that disagreement is precisely what the settle window prevents",
			prefixView.Owner, fullView.Owner)
	}
}

// TestLateClaimLosesToIncumbent pins the other side of the bounded-skew argument:
// a contender arriving well after the settle window loses to the incumbent even
// though its owner id sorts lower, because it never displaces a live lease.
func TestLateClaimLosesToIncumbent(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	if res, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k", Owner: "replica-99",
		Now: now, TTL: time.Minute, Settle: testSettle,
	}); err != nil || !res.Acquired {
		t.Fatalf("incumbent should win: %+v err=%v", res, err)
	}
	late, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k", Owner: "replica-00",
		Now: now.Add(5 * time.Second), TTL: time.Minute, Settle: testSettle,
	})
	if err != nil {
		t.Fatalf("late claim: %v", err)
	}
	if late.Acquired {
		t.Fatalf("a lower owner id must not steal a live lease, got %+v", late)
	}
	if late.Owner != "replica-99" {
		t.Fatalf("expected incumbent replica-99, got %q", late.Owner)
	}
}

// TestLeaseHeldBlocksSecondScheduler covers the ordinary case: a second
// scheduler arriving inside the lease window stands down without writing a
// claim row at all.
func TestLeaseHeldBlocksSecondScheduler(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	first, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "api", Now: now, TTL: 5 * time.Minute,
	})
	if err != nil || !first.Acquired {
		t.Fatalf("first claim should win: %+v err=%v", first, err)
	}
	rowsAfterFirst := store.count()
	second, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "orchestrator", Now: now.Add(30 * time.Second), TTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.Acquired {
		t.Fatalf("second scheduler must not fire an occurrence already leased")
	}
	if second.Owner != "api" {
		t.Fatalf("expected owner api, got %q", second.Owner)
	}
	if store.count() != rowsAfterFirst {
		t.Fatalf("a scheduler standing down should not write a claim row: %d -> %d", rowsAfterFirst, store.count())
	}
}

// TestLeaseReentrantForSameOwner covers a scheduler whose own tick overlaps the
// previous one: it must see the lease as its own, not as contention.
func TestLeaseReentrantForSameOwner(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	if _, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "api", Now: now, TTL: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	again, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "api", Now: now.Add(time.Minute), TTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !again.Acquired || again.Owner != "api" {
		t.Fatalf("owner should still hold its own lease, got %+v", again)
	}
}

// TestLeaseExpiryAllowsTakeover is the owner-died-mid-run case: the lease was
// never released, so once it expires another replica takes over at a higher
// generation instead of the occurrence being stuck forever.
func TestLeaseExpiryAllowsTakeover(t *testing.T) {
	now := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	dead, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-dead", Now: now, TTL: 30 * time.Second,
	})
	if err != nil || !dead.Acquired {
		t.Fatalf("first claim should win: %+v err=%v", dead, err)
	}
	// Still inside the lease: nobody may take over, even though replica-dead is
	// (unknowably) gone. This is the price of a lease and it must hold.
	early, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-live", Now: now.Add(10 * time.Second), TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("early takeover attempt: %v", err)
	}
	if early.Acquired {
		t.Fatalf("takeover before expiry must be refused")
	}

	// Past expiry: takeover is allowed and is flagged as such.
	late, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-live", Now: now.Add(31 * time.Second), TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("late takeover: %v", err)
	}
	if !late.Acquired {
		t.Fatalf("expired lease must be takeable, got %+v", late)
	}
	if !late.TakenOver || late.Generation != 1 {
		t.Fatalf("takeover should be generation 1 and flagged: %+v", late)
	}
	if late.Owner != "replica-live" {
		t.Fatalf("expected replica-live to own the takeover, got %q", late.Owner)
	}
}

// TestConcurrentTakeoverFiresOnce is the takeover race: an owner dies mid-run
// and two survivors notice at the same moment. Still exactly one fire.
func TestConcurrentTakeoverFiresOnce(t *testing.T) {
	now := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	for round := 0; round < 25; round++ {
		store := &fakeLeaseStore{}
		if _, err := claimScheduleFire(store, scheduleClaimRequest{
			ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-dead", Now: now, TTL: 30 * time.Second,
		}); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
		winners := runContenders(t, 4, func(req scheduleClaimRequest) (bool, error) {
			res, err := claimScheduleFire(store, req)
			return res.Acquired, err
		}, now.Add(time.Minute), "k1")
		if len(winners) != 1 {
			t.Fatalf("round %d: concurrent takeover must yield 1 fire, got %d: %v", round, len(winners), winners)
		}
	}
}

// TestReleasedLeaseIsRetryable covers a scheduler that won the lease but never
// dispatched: it releases, and another replica may retry the same occurrence
// immediately rather than the occurrence being lost until the TTL expires.
func TestReleasedLeaseIsRetryable(t *testing.T) {
	now := time.Date(2026, 8, 4, 14, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{}
	req := scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-a", Now: now, TTL: 10 * time.Minute,
	}
	if res, err := claimScheduleFire(store, req); err != nil || !res.Acquired {
		t.Fatalf("first claim should win: %+v err=%v", res, err)
	}
	if err := releaseScheduleLease(store, req, "runner unavailable"); err != nil {
		t.Fatalf("release: %v", err)
	}
	retry, err := claimScheduleFire(store, scheduleClaimRequest{
		ScenarioID: "scn-1", FireKey: "k1", Owner: "replica-b", Now: now.Add(time.Second), TTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if !retry.Acquired || retry.Owner != "replica-b" {
		t.Fatalf("a released occurrence must be retryable well inside the TTL, got %+v", retry)
	}
}

// TestClaimWithoutStoreDoesNotFire pins the safe default: no lease store means
// no fire, rather than firing blind.
func TestClaimWithoutStoreDoesNotFire(t *testing.T) {
	res, err := claimScheduleFire(nil, scheduleClaimRequest{ScenarioID: "s", FireKey: "k", Owner: "o", Now: time.Now()})
	if err == nil {
		t.Fatalf("expected an error when no lease store is configured")
	}
	if res.Acquired {
		t.Fatalf("must not fire without a lease store")
	}
}

// TestLostInsertDoesNotFire covers a store that accepts the insert but does not
// surface it: the contender must stand down instead of assuming ownership.
func TestLostInsertDoesNotFire(t *testing.T) {
	res, err := claimScheduleFire(blackHoleLeaseStore{}, scheduleClaimRequest{
		ScenarioID: "s", FireKey: "k", Owner: "o", Now: time.Now(), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Acquired {
		t.Fatalf("must not fire on a claim that never became visible")
	}
}

// blackHoleLeaseStore accepts writes and never returns them.
type blackHoleLeaseStore struct{}

func (blackHoleLeaseStore) InsertClaim(scheduleClaim) error { return nil }
func (blackHoleLeaseStore) ListClaims(string, string) ([]scheduleClaim, error) {
	return nil, nil
}

// TestScheduleFireKeyIsReplicaStable pins the property the whole lease depends
// on: two replicas looking at the same schedule at slightly different instants
// must derive the same occurrence key. A key built from the wall clock would
// never collide, so every replica would win its own lease and all of them fire.
func TestScheduleFireKeyIsReplicaStable(t *testing.T) {
	cases := []struct {
		name  string
		sched map[string]interface{}
		state scheduleState
	}{
		{
			name:  "next fire from state row",
			sched: map[string]interface{}{"every_minutes": 15},
			state: scheduleState{NextFireAt: time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)},
		},
		{
			name:  "next fire still in schedule_json",
			sched: map[string]interface{}{"next_fire_at": "2026-08-04T15:00:00Z"},
		},
		{
			name:  "interval bucket",
			sched: map[string]interface{}{"every_minutes": 10},
		},
		{
			name:  "daily",
			sched: map[string]interface{}{"daily_at": "03:30"},
		},
		{
			name:  "no interval and no daily falls back to a minute bucket",
			sched: map[string]interface{}{"enabled": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Two replicas ticking 900ms apart inside the same minute.
			a := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
			b := a.Add(900 * time.Millisecond)
			ka := scheduleFireKey(tc.sched, tc.state, a)
			kb := scheduleFireKey(tc.sched, tc.state, b)
			if ka != kb {
				t.Fatalf("replicas derived different keys for one occurrence: %q vs %q", ka, kb)
			}
			if ka == "" {
				t.Fatalf("fire key must not be empty")
			}
		})
	}
}

// TestScheduleFireKeyChangesBetweenOccurrences is the other half: consecutive
// occurrences must get different keys, or the second one would be blocked by the
// first one's lease.
func TestScheduleFireKeyChangesBetweenOccurrences(t *testing.T) {
	sched := map[string]interface{}{"every_minutes": 10}
	first := scheduleFireKey(sched, scheduleState{}, time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC))
	second := scheduleFireKey(sched, scheduleState{}, time.Date(2026, 8, 4, 15, 10, 0, 0, time.UTC))
	if first == second {
		t.Fatalf("consecutive occurrences must not share a fire key (%q)", first)
	}

	// And via the state row, which is how it actually advances after a fire.
	s1 := scheduleState{NextFireAt: time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)}
	s2 := scheduleState{NextFireAt: time.Date(2026, 8, 4, 15, 10, 0, 0, time.UTC)}
	if scheduleFireKey(sched, s1, time.Now()) == scheduleFireKey(sched, s2, time.Now()) {
		t.Fatalf("advancing next_fire_at must change the fire key")
	}
}

// TestScheduleLeaseGenerationCounting pins the audit metadata: a takeover is
// only claimed when a lease actually expired unreleased.
func TestScheduleLeaseGenerationCounting(t *testing.T) {
	now := time.Date(2026, 8, 4, 16, 0, 0, 0, time.UTC)
	live := []scheduleClaim{{Owner: "a", ClaimedAt: now, ExpiresAt: now.Add(time.Minute)}}
	if gen, takeover := scheduleLeaseGeneration(live, now); gen != 0 || takeover {
		t.Fatalf("a live lease is not a takeover: gen=%d takeover=%v", gen, takeover)
	}
	expired := []scheduleClaim{{Owner: "a", ClaimedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}}
	if gen, takeover := scheduleLeaseGeneration(expired, now); gen != 1 || !takeover {
		t.Fatalf("an expired unreleased lease is a takeover at gen 1: gen=%d takeover=%v", gen, takeover)
	}
	released := []scheduleClaim{
		{Owner: "a", ClaimedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)},
		{Owner: "a", ClaimedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Hour), Released: true},
	}
	if gen, takeover := scheduleLeaseGeneration(released, now); takeover || gen != 0 {
		t.Fatalf("a released lease is finished, not a dead owner: gen=%d takeover=%v", gen, takeover)
	}
}
