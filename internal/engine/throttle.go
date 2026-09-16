package engine

import "github.com/gmizrahi/gpsync/internal/statedb"

// ThrottleEventAnalysis is one throttle_events row enriched with the
// derived figures gpsync throttle-log reports: the upload activity in the
// clean window right before it, and how long recovery actually took.
// RecoveryKnown is false until a real upload has landed since -- still
// throttled, or nothing was pending to test with.
type ThrottleEventAnalysis struct {
	statedb.ThrottleEvent
	CleanWindowFiles int
	CleanWindowBytes int64
	RecoveredAt      float64
	RecoveryKnown    bool
	RecoverySeconds  float64
}

// BuildThrottleAnalysis replays db's throttle_events log in order,
// enriching each event with the clean-window activity before it and its
// real recovery time after it -- the one shared implementation behind
// `gpsync throttle-log`'s prose report, --csv, and --analyze modes, and
// the dashboard's Statistics-page view, so "how long did recovery really
// take" is computed exactly once.
//
// Both events and db.SuccessfulUploadTimestamps() are ascending by time,
// and each throttle event's clean window starts exactly where the
// previous one's ended -- a strict, non-overlapping partition of the
// timeline. That means a SINGLE forward pointer through the uploads
// slice, advanced once per event, both sums that event's clean window
// AND lands on exactly the first upload after it (its recovery) in the
// same pass: one walk and one query for the whole ledger, instead of two
// queries PER throttle event (see SuccessfulUploadTimestamps' own doc
// comment).
func BuildThrottleAnalysis(db *statedb.DB) ([]ThrottleEventAnalysis, error) {
	events, err := db.ThrottleEvents()
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	uploads, err := db.SuccessfulUploadTimestamps()
	if err != nil {
		return nil, err
	}

	out := make([]ThrottleEventAnalysis, 0, len(events))
	// No prior event for the first one -- look back a full day for its
	// leading clean window, matching the original report's own choice.
	windowStart := events[0].At - 24*3600
	ptr := 0
	for _, ev := range events {
		var files int
		var bytes int64
		for ptr < len(uploads) && uploads[ptr].At <= ev.At {
			if uploads[ptr].At > windowStart {
				files++
				bytes += uploads[ptr].Size
			}
			ptr++
		}
		a := ThrottleEventAnalysis{ThrottleEvent: ev, CleanWindowFiles: files, CleanWindowBytes: bytes}
		// ptr now sits at the first upload with At > ev.At (the loop above
		// only stops early there, or at the end of the slice) -- exactly
		// what FirstUploadAfter(ev.At) used to look up with its own query.
		if ptr < len(uploads) {
			a.RecoveredAt = uploads[ptr].At
			a.RecoveryKnown = true
			a.RecoverySeconds = a.RecoveredAt - ev.At
		}
		out = append(out, a)
		windowStart = ev.At
	}
	return out, nil
}

// RungRecoveryStats summarizes every known recovery at one backoff rung --
// the "recovery time distribution by rung" the original request asked
// for: "we can start playing with the 'last' window between the last
// backoff/429 and the next successful upload until we find the sweet
// spot."
type RungRecoveryStats struct {
	Rung       int
	Count      int
	MinSeconds float64
	MaxSeconds float64
	AvgSeconds float64
}

// RecoveryStatsByRung groups events's known recoveries by rung, ordered by
// rung ascending. A rung with zero known recoveries is simply absent, not
// a zero-valued entry.
func RecoveryStatsByRung(events []ThrottleEventAnalysis) []RungRecoveryStats {
	byRung := map[int][]float64{}
	maxRung := -1
	for _, e := range events {
		if !e.RecoveryKnown {
			continue
		}
		byRung[e.Rung] = append(byRung[e.Rung], e.RecoverySeconds)
		if e.Rung > maxRung {
			maxRung = e.Rung
		}
	}
	var out []RungRecoveryStats
	for rung := 0; rung <= maxRung; rung++ {
		secs, ok := byRung[rung]
		if !ok {
			continue
		}
		min, max, total := secs[0], secs[0], 0.0
		for _, s := range secs {
			if s < min {
				min = s
			}
			if s > max {
				max = s
			}
			total += s
		}
		out = append(out, RungRecoveryStats{
			Rung:       rung,
			Count:      len(secs),
			MinSeconds: min,
			MaxSeconds: max,
			AvgSeconds: total / float64(len(secs)),
		})
	}
	return out
}
