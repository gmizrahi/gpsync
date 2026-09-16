// Package quota implements quota awareness: the documented 10,000
// requests/project/day ceiling (reset at midnight Pacific Time, correct
// across PST/PDT), plus an adaptive concurrency limiter for the
// undocumented per-minute/concurrent-write throttle Google doesn't publish
// a number for.
package quota

import (
	"sync"
	"time"

	// Embeds the IANA time zone database into the binary. Without this, a
	// standalone Windows .exe has no system tzdata to resolve
	// "America/Los_Angeles" against and LoadLocation would fail at runtime.
	_ "time/tzdata"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

const (
	DailyLimit        = 10_000
	DailySafetyMargin = 200 // stop a bit before the hard ceiling
	pacificZoneName   = "America/Los_Angeles"
)

var pacific = func() *time.Location {
	loc, err := time.LoadLocation(pacificZoneName)
	if err != nil {
		// Should be unreachable thanks to the time/tzdata embed above, but
		// fall back to UTC rather than crash if it ever happens.
		return time.UTC
	}
	return loc
}()

func PacificTodayStr() string {
	return time.Now().In(pacific).Format("2006-01-02")
}

func SecondsUntilPacificMidnight() float64 {
	nowPT := time.Now().In(pacific)
	tomorrow := time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day()+1, 0, 0, 0, 0, pacific)
	return tomorrow.Sub(nowPT).Seconds()
}

// PacificMidnightEpoch returns the Unix epoch (seconds) of the most recent
// Pacific midnight -- the same reset boundary the daily API quota uses
// (PacificTodayStr/QuotaUsed above), reused here so "uploaded today" means
// the same "today" the quota counter already shows on the dashboard and in
// `gpsync info`, not an arbitrary different reset time.
func PacificMidnightEpoch() float64 {
	nowPT := time.Now().In(pacific)
	midnight := time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day(), 0, 0, 0, 0, pacific)
	return float64(midnight.Unix())
}

type DailyQuota struct {
	db         *statedb.DB
	dailyLimit int
}

func NewDailyQuota(db *statedb.DB) *DailyQuota {
	return &DailyQuota{db: db, dailyLimit: DailyLimit}
}

func (q *DailyQuota) Used() (int, error) {
	return q.db.QuotaUsed(PacificTodayStr())
}

func (q *DailyQuota) Remaining() (int, error) {
	used, err := q.Used()
	if err != nil {
		return 0, err
	}
	r := q.dailyLimit - DailySafetyMargin - used
	if r < 0 {
		r = 0
	}
	return r, nil
}

func (q *DailyQuota) Record(n int) error {
	return q.db.QuotaIncrement(PacificTodayStr(), n)
}

// Set overwrites (not adds to) today's tracked usage -- see QuotaSet.
func (q *DailyQuota) Set(n int) error {
	return q.db.QuotaSet(PacificTodayStr(), n)
}

// Exhausted fails OPEN on a read error (treats it as "not exhausted", not
// "exhausted") -- a transient local DB read error is not evidence the
// quota is actually used up, and wrongly stopping an entire run over one is
// worse than the alternative: the reactive 429-handling path (see
// reclassifyDailyQuota in internal/uploader) is the real backstop against
// genuinely exceeding Google's ceiling, so failing open here just means
// falling back to that reactive protection instead of also short-circuiting
// proactively for this one check.
func (q *DailyQuota) Exhausted() bool {
	r, err := q.Remaining()
	if err != nil {
		return false
	}
	return r <= 0
}

func (q *DailyQuota) DailyLimit() int { return q.dailyLimit }

// AdaptiveConcurrency is an AIMD-style concurrency control: shrink hard on a
// 429, grow back slowly on sustained success. Google doesn't document the
// per-minute/concurrent threshold, so this reacts to real throttling
// signals instead of guessing a fixed number.
type AdaptiveConcurrency struct {
	mu                   sync.Mutex
	configuredMax        int
	current              int
	consecutiveSuccesses int
	// lastKnownGood is TCP slow-start's ssthresh: the concurrency level we
	// were demonstrably sustaining immediately before the most recent
	// throttle. Below it, recovery can be aggressive -- that pace was
	// working seconds ago. At or above it we're back in territory that just
	// got us throttled, so growth reverts to the cautious linear rule.
	// Zero until the first throttle, which keeps a fresh limiter (already
	// at configuredMax) on the conservative path.
	lastKnownGood int
}

// Ramp-up rates: consecutive successes required per step, in slow-start
// (doubling, below lastKnownGood) and in congestion avoidance (+1, at or
// above it). Recovering purely linearly meant a halving from 6 to 3 took 60
// successful uploads to undo -- an eternity in the middle of a large sync,
// spent throttling the pipeline well below a rate that was known to work.
const (
	slowStartSuccessStep = 5
	linearSuccessStep    = 20
)

// NewAdaptiveConcurrency starts at 1, not at configuredMax, and seeds
// lastKnownGood to configuredMax so the ramp-up in OnSuccess treats a cold
// start as exactly what it is: a slow start.
//
// Opening a run at full configured concurrency assumes the safe rate is
// already known, and it isn't -- that assumption is what walks straight
// into Google's undocumented "concurrent write request" ceiling on file #1,
// most likely right after a gap long enough for the rate-limit window to
// have reset. Real TCP slow-start never makes that assumption either: it
// opens at one segment and discovers the ceiling by growing until something
// pushes back. Here that costs ~15 files of reduced parallelism before
// reaching a configuredMax of 6, and buys not tripping the limiter that the
// whole circuit breaker exists to recover from.
//
// No separate cold-start code path is needed: current < lastKnownGood is
// already the doubling branch, so 1 → 2 → 4 → ... → configuredMax happens
// by the same rule that recovers from a throttle. Once current reaches
// configuredMax on a clean run it stays there, being at both bounds.
func NewAdaptiveConcurrency(configuredMax int) *AdaptiveConcurrency {
	if configuredMax < 1 {
		configuredMax = 1
	}
	return &AdaptiveConcurrency{
		configuredMax: configuredMax,
		current:       1,
		lastKnownGood: configuredMax,
	}
}

func (a *AdaptiveConcurrency) CurrentLimit() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

func (a *AdaptiveConcurrency) OnSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveSuccesses++
	if a.current >= a.configuredMax {
		return
	}
	// Slow start: below the level we were sustaining before the throttle,
	// double on a short success streak. Never past lastKnownGood in one
	// step -- overshooting straight back into the level that just got
	// throttled is exactly what congestion avoidance exists to prevent.
	if a.current < a.lastKnownGood {
		if a.consecutiveSuccesses >= slowStartSuccessStep {
			next := a.current * 2
			if next > a.lastKnownGood {
				next = a.lastKnownGood
			}
			if next > a.configuredMax {
				next = a.configuredMax
			}
			a.current = next
			a.consecutiveSuccesses = 0
		}
		return
	}
	// Congestion avoidance: at or above the last throttled level, creep.
	if a.consecutiveSuccesses >= linearSuccessStep {
		a.current++
		a.consecutiveSuccesses = 0
	}
}

// OnThrottled halves the concurrency ceiling after a throttle response.
//
// It deliberately computes no delay of its own. HOW LONG to pause after a
// throttle is a run-wide decision, not a per-call one: the uploader's
// circuit breaker stops the whole pipeline and sleeps a fixed escalating
// schedule (see throttleCircuitBreakerSchedule in internal/uploader).
// Per-call delays returned from here used to coexist with concurrent
// per-file retries, which meant every file backed off independently while
// its siblings kept hammering the API -- the aggregate request rate barely
// dropped, so the throttle never had a chance to clear. Shrinking the
// concurrency ceiling is the part that still belongs here; the waiting
// belongs to the breaker.
func (a *AdaptiveConcurrency) OnThrottled() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Remember the level we were sustaining right up to this throttle,
	// before halving -- that's what makes a fast recovery back toward it
	// safe (see lastKnownGood / OnSuccess).
	a.lastKnownGood = a.current
	a.current = a.current / 2
	if a.current < 1 {
		a.current = 1
	}
	a.consecutiveSuccesses = 0
}
