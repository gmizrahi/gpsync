package quota

import (
	"path/filepath"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func openTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	statedb.StateDir = t.TempDir()
	statedb.StateDBPath = filepath.Join(statedb.StateDir, "state.sqlite")
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPacificTodayStr_IsWellFormed(t *testing.T) {
	s := PacificTodayStr()
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		t.Errorf("PacificTodayStr() = %q, want YYYY-MM-DD shape", s)
	}
}

func TestDailyQuota_SetOverwritesRatherThanAdds(t *testing.T) {
	db := openTestDB(t)
	q := NewDailyQuota(db)

	if err := q.Record(300); err != nil {
		t.Fatal(err)
	}
	// Manually correcting for parallel/out-of-band requests: set to an exact value.
	if err := q.Set(750); err != nil {
		t.Fatal(err)
	}
	used, err := q.Used()
	if err != nil {
		t.Fatal(err)
	}
	if used != 750 {
		t.Errorf("used = %d, want 750 (Set must overwrite, not add to the prior 300)", used)
	}

	// Set again to a lower value -- must also work as a correction downward.
	if err := q.Set(100); err != nil {
		t.Fatal(err)
	}
	used, err = q.Used()
	if err != nil {
		t.Fatal(err)
	}
	if used != 100 {
		t.Errorf("used after second Set = %d, want 100", used)
	}
}

func TestDailyQuota_RecordAndRemaining(t *testing.T) {
	db := openTestDB(t)
	q := NewDailyQuota(db)

	remaining0, err := q.Remaining()
	if err != nil {
		t.Fatal(err)
	}
	if remaining0 != DailyLimit-DailySafetyMargin {
		t.Errorf("initial remaining = %d, want %d", remaining0, DailyLimit-DailySafetyMargin)
	}

	if err := q.Record(500); err != nil {
		t.Fatal(err)
	}
	remaining1, err := q.Remaining()
	if err != nil {
		t.Fatal(err)
	}
	if remaining1 != remaining0-500 {
		t.Errorf("remaining after recording 500 = %d, want %d", remaining1, remaining0-500)
	}
}

func TestDailyQuota_ExhaustedNearCeiling(t *testing.T) {
	db := openTestDB(t)
	q := NewDailyQuota(db)

	if q.Exhausted() {
		t.Fatal("should not be exhausted initially")
	}
	if err := q.Record(DailyLimit); err != nil {
		t.Fatal(err)
	}
	if !q.Exhausted() {
		t.Error("expected exhausted after recording the full daily limit")
	}
}

// TestDailyQuota_Exhausted_FailsOpenOnReadError proves Exhausted() fails
// OPEN (returns false) when the local counter can't be read, rather than
// treating a read error as genuine exhaustion. This matters in practice:
// under real upload concurrency, a transient SQLite lock-contention error
// reading this counter was previously misread as "the daily quota is used
// up," silently stopping an entire sync run over a DB hiccup that had
// nothing to do with quota. A false "not exhausted" here is safe -- the
// reactive 429-handling path in internal/uploader is the real backstop
// against genuinely exceeding Google's ceiling.
func TestDailyQuota_Exhausted_FailsOpenOnReadError(t *testing.T) {
	db := openTestDB(t)
	q := NewDailyQuota(db)
	db.Close() // force every subsequent read against this DB to error

	if q.Exhausted() {
		t.Error("Exhausted() = true on a read error, want false (fail open)")
	}
}

// atFullSpeed returns a limiter that has already ramped up to its
// configured maximum. A fresh limiter deliberately cold-starts at 1 and
// slow-starts upward (see NewAdaptiveConcurrency), so tests about THROTTLE
// behavior have to warm it up first: they are about what happens to a run
// already going at full speed, which is where a healthy run spends nearly
// all of its time.
func atFullSpeed(t *testing.T, configuredMax int) *AdaptiveConcurrency {
	t.Helper()
	a := NewAdaptiveConcurrency(configuredMax)
	for i := 0; i < 1000 && a.CurrentLimit() < configuredMax; i++ {
		a.OnSuccess()
	}
	if a.CurrentLimit() != configuredMax {
		t.Fatalf("setup: limiter reached %d, want %d", a.CurrentLimit(), configuredMax)
	}
	return a
}

func TestAdaptiveConcurrency_HalvesOnThrottleAndCapsAtConfigured(t *testing.T) {
	a := atFullSpeed(t, 8)

	a.OnThrottled()
	if a.CurrentLimit() != 4 {
		t.Errorf("after one throttle, limit = %d, want 4", a.CurrentLimit())
	}

	a.OnThrottled()
	if a.CurrentLimit() != 2 {
		t.Errorf("after two throttles, limit = %d, want 2", a.CurrentLimit())
	}

	// Ramp-up (slow start below the pre-throttle level, then linear) must
	// never exceed the configured max.
	for i := 0; i < 100; i++ {
		a.OnSuccess()
	}
	if a.CurrentLimit() > 8 {
		t.Errorf("limit exceeded configured max: %d > 8", a.CurrentLimit())
	}
}

func TestAdaptiveConcurrency_NeverBelowOne(t *testing.T) {
	a := NewAdaptiveConcurrency(1)
	for i := 0; i < 5; i++ {
		a.OnThrottled()
	}
	if a.CurrentLimit() != 1 {
		t.Errorf("limit = %d, want 1 (floor)", a.CurrentLimit())
	}
}

// TestAdaptiveConcurrency_SlowStartRecoversFastBelowTheLastKnownGoodLevel
// proves the fix for a needlessly glacial recovery. Ramp-up used to be flat
// linear -- +1 per 20 consecutive successes regardless of how far the limit
// had fallen -- so a halving from 8 to 4 cost 80 successful uploads to
// undo, most of them spent running at a pace that was demonstrably fine
// moments earlier. Recovery below the pre-throttle level (TCP's ssthresh,
// here lastKnownGood) now doubles on a short success streak instead.
func TestAdaptiveConcurrency_SlowStartRecoversFastBelowTheLastKnownGoodLevel(t *testing.T) {
	a := atFullSpeed(t, 8)
	a.OnThrottled()
	if a.CurrentLimit() != 4 {
		t.Fatalf("setup: limit after one throttle = %d, want 4", a.CurrentLimit())
	}

	successes := 0
	for a.CurrentLimit() < 8 && successes < 200 {
		a.OnSuccess()
		successes++
	}
	if a.CurrentLimit() != 8 {
		t.Fatalf("never recovered to 8 within %d successes (limit=%d)", successes, a.CurrentLimit())
	}
	// The old linear rule needed 4 steps x 20 = 80 successes to get from 4
	// back to 8. Slow start must do it in dramatically fewer.
	const oldLinearCost = 80
	if successes >= oldLinearCost {
		t.Errorf("recovery to the pre-throttle level took %d successes, no better than the old flat linear rule's %d", successes, oldLinearCost)
	}
}

// TestAdaptiveConcurrency_GrowthTurnsConservativeAtTheLastKnownGoodLevel is
// the other half of the slow-start contract: fast growth stops exactly at
// the level that just got throttled. Past that point we're in untested
// territory again and must creep, not double -- otherwise recovery would
// blow straight back through the ceiling that caused the throttle.
func TestAdaptiveConcurrency_GrowthTurnsConservativeAtTheLastKnownGoodLevel(t *testing.T) {
	a := atFullSpeed(t, 16)
	// Drop from 16 to 8 (lastKnownGood = 16), then from 8 to 4
	// (lastKnownGood = 8) -- so the threshold under test is 8, comfortably
	// below the configured max of 16.
	a.OnThrottled()
	a.OnThrottled()
	if a.CurrentLimit() != 4 {
		t.Fatalf("setup: limit = %d, want 4", a.CurrentLimit())
	}

	// Slow start: 4 -> 8 in two doubling steps of 5 successes each.
	for i := 0; i < slowStartSuccessStep; i++ {
		a.OnSuccess()
	}
	if a.CurrentLimit() != 8 {
		t.Fatalf("after %d successes, limit = %d, want 8 (one doubling, clamped at lastKnownGood)", slowStartSuccessStep, a.CurrentLimit())
	}

	// At the threshold now -- growth must revert to +1 per 20.
	for i := 0; i < linearSuccessStep-1; i++ {
		a.OnSuccess()
	}
	if a.CurrentLimit() != 8 {
		t.Errorf("limit = %d after %d successes at the last-known-good level, want 8 -- growth past a just-throttled level must be conservative, not another doubling", a.CurrentLimit(), linearSuccessStep-1)
	}
	a.OnSuccess()
	if a.CurrentLimit() != 9 {
		t.Errorf("limit = %d after %d successes, want 9 (+1 linear step)", a.CurrentLimit(), linearSuccessStep)
	}
}

// TestAdaptiveConcurrency_ColdStartsAtOneAndSlowStartsUp proves a fresh run
// no longer opens at full configured concurrency.
//
// It used to start at configuredMax, i.e. it assumed the safe rate was
// already known -- and blasting Google's undocumented "concurrent write
// request" ceiling at full width from file #1 is exactly how a run trips
// the limiter that the whole circuit breaker then has to recover from,
// especially right after a gap long enough for the rate-limit window to
// reset. TCP slow-start makes no such assumption; neither does this now.
//
// The climb must be the FAST (doubling) branch, not the linear one: this is
// discovery of a ceiling that has not pushed back yet, and the configured
// max is the user's own stated intent, not a guess to creep toward.
func TestAdaptiveConcurrency_ColdStartsAtOneAndSlowStartsUp(t *testing.T) {
	const max = 8
	a := NewAdaptiveConcurrency(max)

	if got := a.CurrentLimit(); got != 1 {
		t.Fatalf("initial limit = %d, want 1 -- a fresh run must not open at full concurrency", got)
	}

	// Each doubling costs one short success streak: 1 -> 2 -> 4 -> 8.
	for _, want := range []int{2, 4, 8} {
		for i := 0; i < slowStartSuccessStep; i++ {
			a.OnSuccess()
		}
		if got := a.CurrentLimit(); got != want {
			t.Fatalf("limit = %d after another %d successes, want %d (doubling, not creeping)", got, slowStartSuccessStep, want)
		}
	}

	// Total cost of reaching the configured max must stay small -- the point
	// is a brief cautious opening, not a permanently throttled run.
	fresh := NewAdaptiveConcurrency(max)
	successes := 0
	for fresh.CurrentLimit() < max && successes < 500 {
		fresh.OnSuccess()
		successes++
	}
	if fresh.CurrentLimit() != max {
		t.Fatalf("never reached the configured max within %d successes (limit=%d)", successes, fresh.CurrentLimit())
	}
	// The linear rule would have needed 7 steps x 20 = 140 successes.
	if successes > 3*slowStartSuccessStep {
		t.Errorf("cold start took %d successes to reach %d, want at most %d -- it is climbing at the slow linear rate, not slow-start's doubling", successes, max, 3*slowStartSuccessStep)
	}

	// And once there, with no throttle ever seen, it simply stays.
	for i := 0; i < 200; i++ {
		fresh.OnSuccess()
	}
	if got := fresh.CurrentLimit(); got != max {
		t.Errorf("limit = %d after a long clean run, want %d (the configured max, never exceeded)", got, max)
	}
}

// TestAdaptiveConcurrency_ColdStartWithMaxOfOne: the degenerate case must
// not need a ramp it can never perform.
func TestAdaptiveConcurrency_ColdStartWithMaxOfOne(t *testing.T) {
	a := NewAdaptiveConcurrency(1)
	if got := a.CurrentLimit(); got != 1 {
		t.Fatalf("initial limit = %d, want 1", got)
	}
	for i := 0; i < 50; i++ {
		a.OnSuccess()
	}
	if got := a.CurrentLimit(); got != 1 {
		t.Errorf("limit = %d, want 1 -- must never exceed the configured max", got)
	}
}

// TestAdaptiveConcurrency_OnThrottled_ShrinksWithoutDictatingADelay pins the
// division of responsibility after throttle handling moved to the
// uploader's run-wide circuit breaker: this type still narrows the pipeline
// on a throttle (that part genuinely is a concurrency decision), but it no
// longer computes a per-call backoff.
//
// The delay it used to return was applied per FILE, so each throttled file
// slept on its own clock while its siblings carried on at full concurrency
// -- the aggregate request rate barely dropped and the rate limit never got
// a chance to clear. How long to pause is now one run-wide decision (see
// throttleCircuitBreakerSchedule in internal/uploader), not N independent
// ones.
func TestAdaptiveConcurrency_OnThrottled_ShrinksWithoutDictatingADelay(t *testing.T) {
	a := atFullSpeed(t, 8)
	a.OnThrottled()
	if got := a.CurrentLimit(); got != 4 {
		t.Errorf("limit after one throttle = %d, want 4", got)
	}
	a.OnThrottled()
	if got := a.CurrentLimit(); got != 2 {
		t.Errorf("limit after two throttles = %d, want 2", got)
	}
	// And it still records the pre-throttle level for slow start.
	for i := 0; i < slowStartSuccessStep; i++ {
		a.OnSuccess()
	}
	if got := a.CurrentLimit(); got != 4 {
		t.Errorf("limit after a success streak = %d, want 4 (slow start back toward the last known good level)", got)
	}
}
