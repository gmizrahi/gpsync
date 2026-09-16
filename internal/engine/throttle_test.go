package engine

import (
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func eventAtRung(rung int) statedb.ThrottleEvent {
	return statedb.ThrottleEvent{Rung: rung}
}

func TestBuildThrottleAnalysis_NoEvents_ReturnsNil(t *testing.T) {
	db := openTestDB(t)
	got, err := BuildThrottleAnalysis(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestBuildThrottleAnalysis_CleanWindowAndRecovery(t *testing.T) {
	db := openTestDB(t)

	mustNoErr(t, db.EnsurePending("h-before", 100, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-before", "media-a", ""))
	if _, err := db.GetUpload("h-before"); err != nil {
		t.Fatal(err)
	}

	mustNoErr(t, db.RecordThrottleEvent(2, 180, "Quota exceeded for quota 'concurrent write request'"))
	// The real timestamp RecordThrottleEvent used is "now" (can't be
	// injected), so read it back rather than assume throttleAt lines up
	// exactly -- what matters for this test is the SHAPE of the analysis,
	// not exact injected timestamps.
	events, err := db.ThrottleEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	mustNoErr(t, db.EnsurePending("h-after", 200, "image/jpeg", "/lib/b.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-after", "media-b", ""))

	got, err := BuildThrottleAnalysis(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	a := got[0]
	// h-before uploaded well within the 24h look-back window before the
	// throttle, so it must show up in the clean-window totals.
	if a.CleanWindowFiles != 1 || a.CleanWindowBytes != 100 {
		t.Errorf("clean window = %d file(s)/%d bytes, want 1/100", a.CleanWindowFiles, a.CleanWindowBytes)
	}
	// h-after uploaded strictly after the throttle -- recovery must be
	// known and point at it.
	if !a.RecoveryKnown {
		t.Fatal("RecoveryKnown = false, want true (h-after uploaded after the throttle)")
	}
	if a.RecoverySeconds < 0 {
		t.Errorf("RecoverySeconds = %v, want >= 0", a.RecoverySeconds)
	}
}

// TestBuildThrottleAnalysis_MultipleEvents_SameUploadIsRecoveryAndNextCleanWindow
// is the sharpest case for the single-forward-pointer rewrite (see
// BuildThrottleAnalysis' own doc comment): one upload landing between two
// throttle events must count as BOTH event 1's recovery AND part of event
// 2's leading clean window -- exactly the boundary a naive "advance past
// what's been consumed" pointer could get wrong.
func TestBuildThrottleAnalysis_MultipleEvents_SameUploadIsRecoveryAndNextCleanWindow(t *testing.T) {
	db := openTestDB(t)

	mustNoErr(t, db.RecordThrottleEvent(0, 30, "first throttle"))

	mustNoErr(t, db.EnsurePending("h-mid", 150, "image/jpeg", "/lib/mid.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-mid", "media-mid", ""))

	mustNoErr(t, db.RecordThrottleEvent(1, 60, "second throttle"))

	mustNoErr(t, db.EnsurePending("h-after", 250, "image/jpeg", "/lib/after.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-after", "media-after", ""))

	got, err := BuildThrottleAnalysis(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	ev1, ev2 := got[0], got[1]
	if !ev1.RecoveryKnown {
		t.Error("event 1: RecoveryKnown = false, want true -- h-mid uploaded after it")
	}
	if ev1.RecoverySeconds < 0 {
		t.Errorf("event 1: RecoverySeconds = %v, want >= 0", ev1.RecoverySeconds)
	}
	if ev2.CleanWindowFiles != 1 || ev2.CleanWindowBytes != 150 {
		t.Errorf("event 2: clean window = %d file(s)/%d bytes, want 1/150 (h-mid, uploaded between the two events)", ev2.CleanWindowFiles, ev2.CleanWindowBytes)
	}
	if !ev2.RecoveryKnown {
		t.Error("event 2: RecoveryKnown = false, want true -- h-after uploaded after it")
	}
	if ev2.RecoverySeconds < 0 {
		t.Errorf("event 2: RecoverySeconds = %v, want >= 0", ev2.RecoverySeconds)
	}
}

func TestBuildThrottleAnalysis_NoUploadSinceThrottle_RecoveryUnknown(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.RecordThrottleEvent(0, 30, "Quota exceeded"))

	got, err := BuildThrottleAnalysis(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].RecoveryKnown {
		t.Error("RecoveryKnown = true, want false -- nothing has uploaded since this throttle")
	}
}

func TestRecoveryStatsByRung_GroupsAndComputesMinMaxAvg(t *testing.T) {
	events := []ThrottleEventAnalysis{
		{ThrottleEvent: eventAtRung(0), RecoveryKnown: true, RecoverySeconds: 30},
		{ThrottleEvent: eventAtRung(0), RecoveryKnown: true, RecoverySeconds: 90},
		{ThrottleEvent: eventAtRung(1), RecoveryKnown: true, RecoverySeconds: 60},
		{ThrottleEvent: eventAtRung(2), RecoveryKnown: false}, // must be excluded entirely
	}
	stats := RecoveryStatsByRung(events)
	if len(stats) != 2 {
		t.Fatalf("len(stats) = %d, want 2 (rung 2 has no known recovery)", len(stats))
	}
	if stats[0].Rung != 0 || stats[0].Count != 2 || stats[0].MinSeconds != 30 || stats[0].MaxSeconds != 90 || stats[0].AvgSeconds != 60 {
		t.Errorf("rung 0 stats = %+v, want {Rung:0 Count:2 Min:30 Max:90 Avg:60}", stats[0])
	}
	if stats[1].Rung != 1 || stats[1].Count != 1 || stats[1].AvgSeconds != 60 {
		t.Errorf("rung 1 stats = %+v, want {Rung:1 Count:1 Avg:60}", stats[1])
	}
}

func TestRecoveryStatsByRung_NoKnownRecoveries_ReturnsNil(t *testing.T) {
	events := []ThrottleEventAnalysis{
		{ThrottleEvent: eventAtRung(0), RecoveryKnown: false},
	}
	if got := RecoveryStatsByRung(events); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}
