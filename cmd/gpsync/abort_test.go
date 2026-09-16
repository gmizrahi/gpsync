package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// captureStdout runs fn with os.Stdout redirected, returning what it
// printed. The folder loops report through plain fmt.Print*, so this is the
// only way to assert on what the user actually sees.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

func throttleAbort() error {
	return &uploader.AbortError{
		Reason:    uploader.AbortThrottleBackoffExhausted,
		Detail:    "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'",
		Waited:    34*time.Minute + 35*time.Second,
		Steps:     7,
		Remaining: 12,
	}
}

func quotaAbort() error {
	return &uploader.AbortError{
		Reason:    uploader.AbortDailyQuota,
		Detail:    "daily API quota exhausted (9900/10000 requests used today)",
		Remaining: 40,
	}
}

// TestUploadFolders_StopsTheWholeInvocationOnAbort proves the signal the
// uploader raises actually reaches and is obeyed by `gpsync upload`'s
// folder-by-folder loop. Both stop conditions -- the throttle circuit
// breaker exhausting its entire backoff schedule, and genuine daily-quota
// exhaustion -- are project-wide: continuing to the next folder would hit
// the identical wall, and in the throttle case would sit through the whole
// ~35-minute ladder again to find that out.
//
// The loop previously only stopped on a hard error, so an aborted folder
// looked like a completed one and the next folder started immediately.
func TestUploadFolders_StopsTheWholeInvocationOnAbort(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"throttle backoff exhausted", throttleAbort()},
		{"daily quota exhausted", quotaAbort()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			folders := []engine.PendingFolder{
				{Path: "/lib/2023", Files: 1},
				{Path: "/lib/2024", Files: 1},
				{Path: "/lib/2025", Files: 1},
			}
			var attempted []string
			var stats uploader.Stats
			var err error
			captureStdout(t, func() {
				stats, err = uploadFolders(folders,
					func(folder string) (bool, error) { return true, nil },
					func(folder string) (uploader.Stats, error) {
						attempted = append(attempted, folder)
						if folder == "/lib/2024" {
							// One file did upload before the run stopped.
							return uploader.Stats{Uploaded: 1}, tc.err
						}
						return uploader.Stats{Uploaded: 2}, nil
					})
			})

			if err != nil {
				t.Fatalf("uploadFolders returned %v -- an aborted run is expected, handled behavior and must NOT surface as a command error (that would print a raw \"Error: ...\" dump and exit non-zero)", err)
			}
			want := []string{"/lib/2023", "/lib/2024"}
			if fmt.Sprint(attempted) != fmt.Sprint(want) {
				t.Errorf("attempted folders = %v, want %v -- /lib/2025 must never be started", attempted, want)
			}
			// Work done before the stop still counts.
			if stats.Uploaded != 3 {
				t.Errorf("Uploaded = %d, want 3 (2 from the first folder + 1 from the aborted one)", stats.Uploaded)
			}
		})
	}
}

// TestUploadFolders_RealErrorsStillFailTheCommand: only the deliberate
// abort signal is swallowed. An actual failure must still propagate so the
// command exits non-zero.
func TestUploadFolders_RealErrorsStillFailTheCommand(t *testing.T) {
	folders := []engine.PendingFolder{{Path: "/lib/2023"}, {Path: "/lib/2024"}}
	boom := errors.New("disk exploded")
	var err error
	captureStdout(t, func() {
		_, err = uploadFolders(folders,
			func(folder string) (bool, error) { return true, nil },
			func(folder string) (uploader.Stats, error) { return uploader.Stats{}, boom })
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the underlying failure to propagate", err)
	}
}

// TestUploadFolders_SkipsFoldersWithNothingLeft: a folder whose pending
// files were already swept up by an ancestor folder's scope produces no
// output at all -- no header over an empty block.
func TestUploadFolders_SkipsFoldersWithNothingLeft(t *testing.T) {
	folders := []engine.PendingFolder{{Path: "/lib/2024"}, {Path: "/lib/2024/jan"}}
	var attempted []string
	out := captureStdout(t, func() {
		_, _ = uploadFolders(folders,
			func(folder string) (bool, error) { return folder == "/lib/2024", nil },
			func(folder string) (uploader.Stats, error) {
				attempted = append(attempted, folder)
				return uploader.Stats{}, nil
			})
	})
	if len(attempted) != 1 || attempted[0] != "/lib/2024" {
		t.Errorf("attempted = %v, want only /lib/2024", attempted)
	}
	if strings.Contains(out, "jan") {
		t.Errorf("a folder with nothing to do still printed a header:\n%s", out)
	}
}

// TestSyncFolders_StopsTheWholeInvocationOnAbort is the same contract for
// `gpsync sync`'s loop, which is the command that actually walks many folders
// in one go and therefore the one where continuing past an
// account-wide limit wastes the most time.
func TestSyncFolders_StopsTheWholeInvocationOnAbort(t *testing.T) {
	units := []string{"/lib/2023", "/lib/2024", "/lib/2025"}
	var attempted []string
	var totals syncTotals
	var err error
	captureStdout(t, func() {
		totals, err = syncFolders(units, func(folder string) (syncTotals, error) {
			attempted = append(attempted, folder)
			if folder == "/lib/2024" {
				return syncTotals{synced: 3, total: 15, failedRetry: 12}, throttleAbort()
			}
			return syncTotals{synced: 10, total: 10}, nil
		})
	})

	if err != nil {
		t.Fatalf("syncFolders returned %v -- the abort must not surface as a command error", err)
	}
	want := []string{"/lib/2023", "/lib/2024"}
	if fmt.Sprint(attempted) != fmt.Sprint(want) {
		t.Errorf("attempted folders = %v, want %v -- /lib/2025 must never be started", attempted, want)
	}
	// The aborted folder's real counts are still reported.
	if totals.synced != 13 || totals.total != 25 || totals.failedRetry != 12 {
		t.Errorf("totals = %+v, want synced=13 total=25 failedRetry=12 (the aborted folder's work counts too)", totals)
	}
}

func TestSyncFolders_RealErrorsStillFailTheCommand(t *testing.T) {
	boom := errors.New("scan failed")
	var err error
	captureStdout(t, func() {
		_, err = syncFolders([]string{"/lib/2023"}, func(folder string) (syncTotals, error) {
			return syncTotals{}, boom
		})
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the underlying failure to propagate", err)
	}
}

// TestPrintRunAborted_ExplainsWhatHappenedAndWhatToDo checks the message a
// stopped run leaves on screen. This is expected behavior, not a crash, so
// it must read as an explanation -- what stopped it, what was left, that
// the remaining folders are being skipped on purpose, and that nothing is
// lost -- rather than as a bare error.
func TestPrintRunAborted_ExplainsWhatHappenedAndWhatToDo(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	t.Run("throttle", func(t *testing.T) {
		out := captureStdout(t, func() { printRunAborted(throttleAbort(), 2, 5) })
		for _, want := range []string{
			"rate-limiting",                 // what stopped it
			"7 backoff steps",               // how hard it tried
			"34m35s",                        // for how long
			"12 file(s)",                    // what was left unfinished here
			"remaining 3 folder",            // why nothing else will run
			"Re-run the same command later", // what to do
		} {
			if !strings.Contains(out, want) {
				t.Errorf("aborted-run message is missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Error:") {
			t.Errorf("the message reads as a crash rather than expected behavior:\n%s", out)
		}
	})

	t.Run("daily quota", func(t *testing.T) {
		out := captureStdout(t, func() { printRunAborted(quotaAbort(), 1, 4) })
		for _, want := range []string{
			"daily API quota exhausted",
			"midnight Pacific Time",
			"remaining 3 folder",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("aborted-run message is missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("last folder: nothing to skip", func(t *testing.T) {
		out := captureStdout(t, func() { printRunAborted(throttleAbort(), 5, 5) })
		if strings.Contains(out, "Skipping the remaining") {
			t.Errorf("nothing was left to skip, but the message says otherwise:\n%s", out)
		}
	})

	t.Run("a plain error still prints", func(t *testing.T) {
		out := captureStdout(t, func() { printRunAborted(uploader.ErrRunAborted, 1, 2) })
		if !strings.Contains(out, "Stopped:") {
			t.Errorf("an abort with no detail printed nothing useful:\n%s", out)
		}
	})
}

// openInfoTestDB points statedb at an isolated temp dir and opens it.
func openInfoTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPrintLastRunOutcome_OnlySurfacesWhatIsActionable proves `gpsync info`'s
// new section stays quiet for the ordinary case and speaks up for the ones
// the user may need to act on. A "last run completed" line every single
// time would just be noise to scroll past; an abandoned or interrupted run
// is the opposite -- it means work is outstanding for a reason that had no
// visible trace at all before this.
func TestPrintLastRunOutcome_OnlySurfacesWhatIsActionable(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	cases := []struct {
		name     string
		run      *statedb.RunProgress
		wantShow bool
		contains []string
	}{
		{
			name:     "no run recorded yet",
			run:      nil,
			wantShow: false,
		},
		{
			name:     "a run predating outcome tracking",
			run:      &statedb.RunProgress{Status: "done"},
			wantShow: false,
		},
		{
			name:     "completed normally",
			run:      &statedb.RunProgress{Status: "done", EndReason: statedb.EndReasonCompleted},
			wantShow: false,
		},
		{
			name: "gave up on a throttle",
			run: &statedb.RunProgress{
				Status:    "stopped",
				EndReason: statedb.EndReasonAbortedThrottle,
				EndDetail: "gave up after 34m35s of escalating backoff across 7 steps — 44 file(s) left pending (will retry next run)",
			},
			wantShow: true,
			contains: []string{"Last run:", "aborted (throttle)", "44 file(s) left pending"},
		},
		{
			name: "daily quota exhausted",
			run: &statedb.RunProgress{
				Status:    "stopped",
				EndReason: statedb.EndReasonAbortedDailyQuota,
				EndDetail: "daily API quota exhausted (9850/10000 requests used today) — 40 file(s) left pending (will retry next run)",
			},
			wantShow: true,
			contains: []string{"aborted (daily quota)", "9850/10000"},
		},
		{
			name: "interrupted",
			run: &statedb.RunProgress{
				Status:    "stopped",
				EndReason: statedb.EndReasonInterrupted,
				EndDetail: "stopped by Ctrl+C after 12 of 400 file(s)",
			},
			wantShow: true,
			contains: []string{"interrupted", "12 of 400"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { printLastRunOutcome(tc.run) })
			if !tc.wantShow {
				if strings.TrimSpace(out) != "" {
					t.Errorf("expected no output for this case, got:\n%s", out)
				}
				return
			}
			if strings.TrimSpace(out) == "" {
				t.Fatal("expected the last-run section to be printed")
			}
			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestMarkRunInterrupted_RecordsOnlyAnInProgressRun proves the Ctrl+C
// bookkeeping: a run actually in progress gets annotated so it stops being
// stuck at status='running' forever with nothing recorded, while a signal
// arriving when nothing is running must NOT rewrite the outcome of whatever
// ran last.
//
// This tests the DB-write helper directly rather than real signal delivery:
// the handler is a two-line select around this call, and driving actual
// SIGINT through the test binary would be both flaky and liable to kill the
// test run itself.
func TestMarkRunInterrupted_RecordsOnlyAnInProgressRun(t *testing.T) {
	t.Run("a run in progress is recorded", func(t *testing.T) {
		db := openInfoTestDB(t)
		if err := db.RunStart("upload", 0, 400); err != nil {
			t.Fatal(err)
		}
		done := 12
		if err := db.RunUpdate(nil, &done); err != nil {
			t.Fatal(err)
		}

		markRunInterrupted(db)

		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run.EndReason != statedb.EndReasonInterrupted {
			t.Errorf("EndReason = %q, want %q", run.EndReason, statedb.EndReasonInterrupted)
		}
		if run.Status == "running" {
			t.Error("row is still marked running -- this is exactly the stuck state the handler exists to prevent")
		}
		if !strings.Contains(run.EndDetail, "12 of 400") {
			t.Errorf("EndDetail = %q, want it to record how far the run got", run.EndDetail)
		}
	})

	t.Run("a finished run is left alone", func(t *testing.T) {
		db := openInfoTestDB(t)
		if err := db.RunStart("upload", 0, 10); err != nil {
			t.Fatal(err)
		}
		if err := db.RunEnd(statedb.EndReasonAbortedThrottle, "gave up after backoff"); err != nil {
			t.Fatal(err)
		}

		markRunInterrupted(db) // Ctrl+C at an idle moment

		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run.EndReason != statedb.EndReasonAbortedThrottle {
			t.Errorf("EndReason = %q -- a signal with nothing running must not overwrite the previous run's outcome", run.EndReason)
		}
		if run.EndDetail != "gave up after backoff" {
			t.Errorf("EndDetail = %q, want the original detail preserved", run.EndDetail)
		}
	})

	t.Run("no run at all is harmless", func(t *testing.T) {
		db := openInfoTestDB(t)
		markRunInterrupted(db) // must not panic or error
		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run != nil {
			t.Errorf("expected no run row to be invented, got %+v", run)
		}
	})
}
