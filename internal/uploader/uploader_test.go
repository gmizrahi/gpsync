package uploader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// fakeGooglePhotos is a minimal in-memory stand-in for the real API,
// enough to exercise the full upload -> batchCreate pipeline.
type fakeGooglePhotos struct {
	mu          sync.Mutex
	uploadOrder []int64 // X-Goog-Upload-Raw-Size on each "start" request, in arrival order
}

func newFakeGooglePhotos() *fakeGooglePhotos {
	return &fakeGooglePhotos{}
}

func (f *fakeGooglePhotos) server() *httptest.Server {
	mux := http.NewServeMux()
	// srv is referenced by the /v1/uploads handler below but only read once
	// a request actually arrives, by which point it's been assigned (see
	// httptest.NewServer call at the bottom of this method) -- safe closure.
	var srv *httptest.Server

	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") == "start" {
			if size, err := strconv.ParseInt(r.Header.Get("X-Goog-Upload-Raw-Size"), 10, 64); err == nil {
				f.mu.Lock()
				f.uploadOrder = append(f.uploadOrder, size)
				f.mu.Unlock()
			}
			w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unexpected", 400)
	})

	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Echo the uploaded bytes back as the "upload token" -- unique per file content.
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})

	srv = httptest.NewServer(mux)
	return srv
}

func openTestDB(t *testing.T) *statedb.DB {
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

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploader_Run_HappyPath_UploadsFiles(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	// Point the upload-session URL at the real test server (httptest assigns
	// the port only once the server starts, so the handler can't know its
	// own URL in advance -- easiest fix: same-origin relative redirect).
	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jpg", []byte("content-a"))
	p2 := writeFile(t, dir, "b.jpg", []byte("content-b"))

	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p1, nil))
	mustDB(t, db.EnsurePending("hash-b", 9, "image/jpeg", p2, nil))
	mustDB(t, db.UpsertFileSeen(p1, "hash-a", 0, 9))
	mustDB(t, db.UpsertFileSeen(p2, "hash-b", 0, 9))

	cfg := config.Defaults()

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 2 {
		t.Errorf("Uploaded = %d, want 2", stats.Uploaded)
	}
	if stats.FailedPermanent != 0 || stats.FailedRetryable != 0 {
		t.Errorf("unexpected failures: %+v", stats)
	}

	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["uploaded"] != 2 {
		t.Errorf("counts = %+v, want 2 uploaded", counts)
	}

	uA, err := db.GetUpload("hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if !uA.GoogleMediaItemID.Valid || uA.GoogleMediaItemID.String != "media-content-a" {
		t.Errorf("unexpected media item id: %+v", uA.GoogleMediaItemID)
	}
}

// TestUploader_Run_SmallestFirst_DispatchesInAscendingSizeOrder proves
// cfg.SortSmallestFirst reorders the dispatch queue by size, not just
// ListPendingUnder's default first_source_path order. Concurrency pinned
// to 1 so dispatch order is the only thing that can determine arrival
// order at the fake server -- with more than one in flight, scheduling
// alone could reorder arrivals even with the right queue order.
func TestUploader_Run_SmallestFirst_DispatchesInAscendingSizeOrder(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	// Named so default (path) order would dispatch big -> medium -> small --
	// the opposite of what --smallest-first should produce.
	big := writeFile(t, dir, "a_big.jpg", []byte(strings.Repeat("B", 300)))
	medium := writeFile(t, dir, "b_medium.jpg", []byte(strings.Repeat("M", 150)))
	small := writeFile(t, dir, "c_small.jpg", []byte(strings.Repeat("S", 10)))

	mustDB(t, db.EnsurePending("hash-big", 300, "image/jpeg", big, nil))
	mustDB(t, db.EnsurePending("hash-medium", 150, "image/jpeg", medium, nil))
	mustDB(t, db.EnsurePending("hash-small", 10, "image/jpeg", small, nil))

	cfg := config.Defaults()
	cfg.SortSmallestFirst = true

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
	}

	if _, err := u.Run(); err != nil {
		t.Fatal(err)
	}

	want := []int64{10, 150, 300}
	fake.mu.Lock()
	got := append([]int64(nil), fake.uploadOrder...)
	fake.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("got %d upload starts, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispatch order = %v, want ascending %v", got, want)
			break
		}
	}
}

// TestUploader_Run_QuotaTracksOnlyStructuredAPICalls proves the fix for a
// local daily-quota counter that ran far ahead of Google's real reported
// usage: only structured Library API calls (batchCreate, albums.*) count
// against the local tracker now, not the raw upload-bytes start/finalize
// calls that precede batchCreate for every file. Two files, no album
// strategy (so no album calls), both small enough to land in one
// batchCreate flush -- the tracker should read exactly 1 (one batchCreate
// call), not 5 (2 files * 2 raw-upload calls each + 1 batchCreate, the old
// over-counted total).
func TestUploader_Run_QuotaTracksOnlyStructuredAPICalls(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jpg", []byte("content-a"))
	p2 := writeFile(t, dir, "b.jpg", []byte("content-b"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p1, nil))
	mustDB(t, db.EnsurePending("hash-b", 9, "image/jpeg", p2, nil))

	cfg := config.Defaults()

	dq := quota.NewDailyQuota(db)
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  dq,
		concurrency: quota.NewAdaptiveConcurrency(6),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 2 {
		t.Fatalf("Uploaded = %d, want 2", stats.Uploaded)
	}

	used, err := dq.Used()
	if err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Errorf("local quota tracker after 2 uploaded files (1 batchCreate call, no albums) = %d, want 1 -- raw byte-upload start/finalize calls must not be counted against it", used)
	}
}

func TestUploader_Run_OnBytesReportsFullFileProgress(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	content := []byte("some file content used to verify byte-level progress reporting")
	p := writeFile(t, dir, "a.jpg", content)
	mustDB(t, db.EnsurePending("hash-bytes", int64(len(content)), "image/jpeg", p, nil))

	var mu sync.Mutex
	var events []ByteProgressEvent
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onBytes: func(e ByteProgressEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", stats.Uploaded)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one ByteProgressEvent")
	}

	last := events[len(events)-1]
	if last.Path != p {
		t.Errorf("event path = %q, want %q", last.Path, p)
	}
	if last.Total != int64(len(content)) {
		t.Errorf("event total = %d, want %d", last.Total, len(content))
	}
	if last.Sent != int64(len(content)) {
		t.Errorf("final cumulative sent = %d, want %d (full file)", last.Sent, len(content))
	}
}

func TestUploader_Run_UnsupportedExtension_FiresOnSkipImmediately(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "sidecar.xmp", []byte("xmp-data"))
	mustDB(t, db.EnsurePending("hash-xmp", 8, "application/octet-stream", p, nil))

	var skippedPaths, skippedReasons []string
	u := &Uploader{
		client:      &http.Client{},
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onSkip: func(path, reason string) {
			skippedPaths = append(skippedPaths, path)
			skippedReasons = append(skippedReasons, reason)
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.FailedPermanent != 1 {
		t.Errorf("FailedPermanent = %d, want 1", stats.FailedPermanent)
	}
	if len(skippedPaths) != 1 || skippedPaths[0] != p {
		t.Fatalf("onSkip paths = %v, want [%s]", skippedPaths, p)
	}
	if len(skippedReasons) != 1 || skippedReasons[0] == "" {
		t.Errorf("expected a non-empty skip reason, got %v", skippedReasons)
	}
}

// TestUploader_Run_MediaTypeFilter_LeavesTheOtherKindPendingUntouched
// proves --media-type filtering (cfg.MediaTypeFilter) is a scoping choice
// for THIS run, not a rejection: a video sitting in the same pending batch
// as a photo must be completely untouched -- still 'pending', no ledger
// write, no attempt hash count -- when filtering to photos only, so a
// later `gpsync upload --media-type videos` (or a plain `gpsync upload`) picks
// it up normally.
func TestUploader_Run_MediaTypeFilter_LeavesTheOtherKindPendingUntouched(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	photo := writeFile(t, dir, "a.jpg", []byte("content-photo"))
	video := writeFile(t, dir, "b.mp4", []byte("content-video"))
	mustDB(t, db.EnsurePending("hash-photo", 13, "image/jpeg", photo, nil))
	mustDB(t, db.EnsurePending("hash-video", 13, "video/mp4", video, nil))

	cfg := config.Defaults()
	cfg.MediaTypeFilter = extensions.KindPhoto

	var skipped []string
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onSkip: func(path, reason string) {
			skipped = append(skipped, path+": "+reason)
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 (only the photo)", stats.Uploaded)
	}
	if stats.SkippedMediaType != 1 {
		t.Errorf("SkippedMediaType = %d, want 1", stats.SkippedMediaType)
	}
	if stats.FailedPermanent != 0 {
		t.Errorf("FailedPermanent = %d, want 0 -- the video was scoped out, not rejected", stats.FailedPermanent)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "photo only") {
		t.Errorf("expected one onSkip naming the media-type filter, got: %v", skipped)
	}

	photoRow, err := db.GetUpload("hash-photo")
	if err != nil || photoRow == nil || photoRow.Status != "uploaded" {
		t.Errorf("hash-photo = %+v, err=%v, want uploaded", photoRow, err)
	}
	videoRow, err := db.GetUpload("hash-video")
	if err != nil || videoRow == nil {
		t.Fatal(err)
	}
	if videoRow.Status != "pending" {
		t.Errorf("hash-video status = %q, want unchanged 'pending' -- filtering out must never touch the row", videoRow.Status)
	}
	if videoRow.AttemptCount != 0 {
		t.Errorf("hash-video AttemptCount = %d, want 0 -- never actually attempted", videoRow.AttemptCount)
	}
}

// TestUploader_Run_TransientFailure_RetriesWithinSameRun_AndSucceeds proves
// the in-run bounded retry actually retries: the server fails the *first*
// attempt at starting the upload session (a transient 503) and succeeds on
// the second, all within a single Run() call.
// TestUploader_Run_FastFilesDoNotWaitOnSlowSibling proves the reported bug
// is fixed: with a wave-based dispatcher, all files in the same batch had
// to finish (via WaitGroup.Wait) before ANY of them were reported done --
// so a single large/slow file held up marking its faster batch-mates
// uploaded even though they'd long since finished transferring. Here one
// file's finalize response is deliberately delayed while its siblings
// respond immediately, all under a concurrency limit that fits them in the
// same batch; the fast files must be reported (via onProgress) well before
// the slow one finishes, not held hostage waiting for it.
func TestUploader_Run_FastFilesDoNotWaitOnSlowSibling(t *testing.T) {
	// Shortened from the real 30s default (see batchFlushInterval's own
	// doc comment for why it's 30s now, not the 1s this test was
	// originally written against) -- this test only cares about the
	// wave-vs-flush-ticker BEHAVIOR, not the real production interval, and
	// at 30s it would take minutes to run.
	origFlushInterval := batchFlushInterval
	batchFlushInterval = 50 * time.Millisecond
	t.Cleanup(func() { batchFlushInterval = origFlushInterval })

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	// The slow file's transfer blocks until both fast files have been
	// reported done, or until the test gives up. That makes the ordering
	// guarantee explicit instead of inferring it from elapsed time, which
	// made this test flaky whenever the machine was busy.
	fastReported := make(chan struct{})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "slow-content" {
			select {
			case <-fastReported:
			case <-time.After(10 * time.Second):
				t.Error("timed out waiting for the fast files to be reported while the slow one was still uploading -- they are wave-blocked behind it")
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	slowPath := writeFile(t, dir, "slow.jpg", []byte("slow-content"))
	fast1Path := writeFile(t, dir, "fast1.jpg", []byte("fast-1"))
	fast2Path := writeFile(t, dir, "fast2.jpg", []byte("fast-2"))
	mustDB(t, db.EnsurePending("hash-slow", 12, "image/jpeg", slowPath, nil))
	mustDB(t, db.EnsurePending("hash-fast1", 6, "image/jpeg", fast1Path, nil))
	mustDB(t, db.EnsurePending("hash-fast2", 6, "image/jpeg", fast2Path, nil))

	var mu sync.Mutex
	done := map[string]bool{}
	var slowStillPending bool
	var closeOnce sync.Once
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 4}, // all 3 files fit in one old-style "wave"
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 4),
		onProgress: func(e ProgressEvent) {
			if e.LastFile == "" {
				return
			}
			mu.Lock()
			name := filepath.Base(e.LastFile)
			done[name] = true
			bothFast := done["fast1.jpg"] && done["fast2.jpg"]
			if bothFast && !done["slow.jpg"] {
				slowStillPending = true
			}
			mu.Unlock()
			if bothFast {
				// Let the slow transfer finish now that the point is proven.
				closeOnce.Do(func() { close(fastReported) })
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 3 {
		t.Fatalf("Uploaded = %d, want 3", stats.Uploaded)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"fast1.jpg", "fast2.jpg", "slow.jpg"} {
		if !done[name] {
			t.Errorf("%s was never reported done", name)
		}
	}
	if !slowStillPending {
		t.Error("the fast files were only reported after the slow one finished -- a slow file must not hold up its faster batch-mates")
	}
}

// TestUploader_Run_MisclassifiedDailyQuota_ReclassifiedAndRetried is the
// end-to-end regression test for the original production bug: a transient
// per-minute/burst 429 whose wording merely mentions "daily" must not stop
// the run -- it has to be retried as the throttle it actually is.
//
// The layer that catches it has since moved. retryx.ClassifyResponse now
// only accepts the explicit "per day" wording Google uses for a real daily
// limit, so this message is classified as Throttle outright and never
// reaches reclassifyDailyQuota at all. The test is kept exactly as it was,
// against the real observed wording, because it pins the OUTCOME the user
// cares about (this response retries and succeeds) independently of which
// layer delivers it -- if a future change to the classifier let "daily"
// back in, this would fail again.
//
// The cross-validation layer itself still exists as defense in depth and is
// covered separately by
// TestUploader_Run_GenuineDailyQuotaWording_ButLowLocalUsage_IsReclassified
// and TestReclassifyDailyQuota_ThresholdBoundary.
func TestUploader_Run_MisclassifiedDailyQuota_ReclassifiedAndRetried(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			// Wording that a naive substring check reads as "daily" even
			// though this is really a transient per-minute/burst throttle.
			fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded, this may affect your daily usage patterns, please slow down"}}`)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	captureBackoffs(t) // the retry now goes through the circuit breaker's 5s first rung

	db := openTestDB(t)
	// A realistic "we're nowhere near the real ceiling" local count, like
	// the 1,843 that triggered this bug report in practice.
	mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 1843))

	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("real-content"))
	mustDB(t, db.EnsurePending("hash-misclassified", 12, "image/jpeg", p, nil))

	var throttleCalls int
	var dailyQuotaCalls int
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			if backoffSeconds > 0 {
				throttleCalls++
			} else {
				dailyQuotaCalls++
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1 (should have been reclassified as throttle and retried)", stats.Uploaded)
	}
	if u.quotaHitThisRun.Load() {
		t.Error("quotaHitThisRun must not be set for a reclassified (misclassified) daily-quota response")
	}
	if throttleCalls == 0 {
		t.Error("expected onThrottle to fire for the reclassified throttle event")
	}
	if dailyQuotaCalls != 0 {
		t.Errorf("dailyQuotaCalls = %d, want 0 -- this should never have been treated as genuine daily exhaustion", dailyQuotaCalls)
	}
}

// TestUploader_Run_GenuineDailyQuota_StillRespected proves the
// reclassification fix doesn't overcorrect: when the local counter really
// is near the ceiling, a daily-quota response must still be trusted and
// stop the run (not endlessly retry against a genuinely exhausted quota).
func TestUploader_Run_GenuineDailyQuota_StillRespected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota metric 'requests' and limit 'requests per day'"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL := uploadURL
	uploadURL = srv.URL + "/v1/uploads"
	defer func() { uploadURL = origUploadURL }()

	db := openTestDB(t)
	// Local usage well above dailyQuotaTrustThreshold, so reclassifyDailyQuota
	// trusts the server's daily-quota claim rather than downgrading it -- but
	// not so close to the ceiling that Run()'s own upfront Exhausted() check
	// (remaining <= 0) short-circuits before ever making a request; this needs
	// to exercise the *reactive* 429-handling path, not the proactive skip.
	mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 9000))

	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jpg", []byte("content-a"))
	p2 := writeFile(t, dir, "b.jpg", []byte("content-b"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p1, nil))
	mustDB(t, db.EnsurePending("hash-b", 9, "image/jpeg", p2, nil))

	var dailyQuotaCalls int
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			if backoffSeconds == 0 {
				dailyQuotaCalls++
			}
		},
	}

	stats, err := u.Run()
	if !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted -- genuine daily-quota exhaustion is a per-PROJECT ceiling, so every remaining folder in this invocation would hit the identical wall and the caller must be told to stop", err)
	}
	var abort *AbortError
	if !errors.As(err, &abort) || abort.Reason != AbortDailyQuota {
		t.Errorf("abort reason = %+v, want AbortDailyQuota", abort)
	}
	if stats.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0", stats.Uploaded)
	}
	if !u.quotaHitThisRun.Load() {
		t.Error("expected quotaHitThisRun to be set for a genuine, locally-corroborated daily quota exhaustion")
	}
	if dailyQuotaCalls == 0 {
		t.Error("expected onThrottle to fire for the genuine daily-quota event")
	}
}

// TestUploader_Run_ProactiveQuotaExhaustion_AnnouncedNotSilent proves the
// fix for a completely silent stop: the dispatcher's own upfront check
// against the local daily-request counter (Exhausted(), checked before
// ever sending a request -- independent of any HTTP 429 response) used to
// break the dispatch loop with no callback of any kind: no error, no
// throttle line, nothing to explain why uploads just stopped mid-folder.
// It must now announce itself through onThrottle exactly like the
// reactive 429-handling path (handleError's DailyQuota case) already did.
func TestUploader_Run_ProactiveQuotaExhaustion_AnnouncedNotSilent(t *testing.T) {
	db := openTestDB(t)
	// Remaining = 10000 - 200 - 9800 = 0 -- Exhausted() is true before a
	// single request is ever sent, so this exercises the proactive path,
	// not the reactive 429-handling one.
	mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 9800))

	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jpg", []byte("content-a"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p1, nil))

	var throttleCalls int
	var sawZeroBackoff bool
	u := &Uploader{
		client:      http.DefaultClient, // never actually dialed -- Exhausted() trips before any request goes out
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			throttleCalls++
			if backoffSeconds == 0 {
				sawZeroBackoff = true
			}
		},
	}

	stats, err := u.Run()
	if !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted -- the caller must stop processing further folders", err)
	}
	if stats.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0", stats.Uploaded)
	}
	if stats.SkippedQuota != 1 {
		t.Errorf("SkippedQuota = %d, want 1", stats.SkippedQuota)
	}
	if throttleCalls == 0 {
		t.Fatal("expected onThrottle to fire for the proactive exhaustion check -- this used to be completely silent")
	}
	if !sawZeroBackoff {
		t.Error("expected a genuine (backoffSeconds==0) onThrottle call, not a retry-style one")
	}
}

func TestUploader_Run_TransientFailure_RetriesWithinSameRun_AndSucceeds(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"backend unavailable, try again"}}`)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("retry-me"))
	mustDB(t, db.EnsurePending("hash-retry", 8, "image/jpeg", p, nil))

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1 (should have retried the 503 and succeeded)", stats.Uploaded)
	}

	mu.Lock()
	finalAttempts := attempts
	mu.Unlock()
	if finalAttempts != 2 {
		t.Errorf("server received %d start-upload attempts, want exactly 2 (one failure, one retry)", finalAttempts)
	}

	upload, err := db.GetUpload("hash-retry")
	if err != nil {
		t.Fatal(err)
	}
	if upload.Status != "uploaded" {
		t.Errorf("ledger status = %q, want uploaded", upload.Status)
	}
}

// TestUploader_Run_ExhaustedRetries_RequeuedAndSucceedsOnNextRun proves the
// cross-run retry path: when both in-run attempts fail, the row lands as
// failed_retryable -- and the *next* Run() call (RequeueRetryable) picks it
// back up and retries it, succeeding once the server recovers.
func TestUploader_Run_ExhaustedRetries_RequeuedAndSucceedsOnNextRun(t *testing.T) {
	var mu sync.Mutex
	failing := true // both attempts in run 1 fail; run 2 succeeds

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failing
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"backend unavailable"}}`)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("requeue-me"))
	mustDB(t, db.EnsurePending("hash-requeue", 10, "image/jpeg", p, nil))

	newUploader := func() *Uploader {
		return &Uploader{
			client:      srv.Client(),
			db:          db,
			cfg:         config.Defaults(),
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(6),
		}
	}

	// Run 1: server down the whole time -- both bounded attempts fail.
	stats1, err := newUploader().Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats1.Uploaded != 0 || stats1.FailedRetryable != 1 {
		t.Fatalf("run 1 stats = %+v, want 0 uploaded / 1 failed_retryable", stats1)
	}
	upload, err := db.GetUpload("hash-requeue")
	if err != nil {
		t.Fatal(err)
	}
	if upload.Status != "failed_retryable" {
		t.Fatalf("status after run 1 = %q, want failed_retryable", upload.Status)
	}

	// Server recovers; run 2 must requeue the failed_retryable row and succeed.
	mu.Lock()
	failing = false
	mu.Unlock()

	stats2, err := newUploader().Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Uploaded != 1 {
		t.Fatalf("run 2 stats = %+v, want 1 uploaded (requeue should have retried it)", stats2)
	}
	upload, err = db.GetUpload("hash-requeue")
	if err != nil {
		t.Fatal(err)
	}
	if upload.Status != "uploaded" {
		t.Errorf("status after run 2 = %q, want uploaded", upload.Status)
	}
}

func TestUploader_Run_PermanentFailure_NoRetryLoop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT","message":"corrupt file"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL := uploadURL
	uploadURL = srv.URL + "/v1/uploads"
	defer func() { uploadURL = origUploadURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "bad.jpg", []byte("corrupt"))
	mustDB(t, db.EnsurePending("hash-bad", 7, "image/jpeg", p, nil))

	cfg := config.Defaults()

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.FailedPermanent != 1 {
		t.Errorf("FailedPermanent = %d, want 1", stats.FailedPermanent)
	}

	failures, err := db.ListFailures(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Status != "failed_permanent" {
		t.Errorf("unexpected failures: %+v", failures)
	}
}

func mustDB(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// rampedConcurrency returns an AdaptiveConcurrency already at its
// configured maximum. A fresh limiter deliberately cold-starts at 1 and
// slow-starts upward (see quota.NewAdaptiveConcurrency), so a test whose
// premise is "several files genuinely in flight at once" has to warm it up
// first -- otherwise it silently becomes a serial test that still passes
// while proving nothing about concurrency.
func rampedConcurrency(t *testing.T, max int) *quota.AdaptiveConcurrency {
	t.Helper()
	c := quota.NewAdaptiveConcurrency(max)
	for i := 0; i < 1000 && c.CurrentLimit() < max; i++ {
		c.OnSuccess()
	}
	if c.CurrentLimit() != max {
		t.Fatalf("setup: concurrency limiter reached %d, want %d", c.CurrentLimit(), max)
	}
	return c
}

// backoffRecorder replaces the package-level sleep hook so a test can prove
// WHAT would have been slept without actually waiting. The circuit-breaker
// schedule tops out at 15 minutes per rung and is nearly an hour end to
// end, so driving the clock is the only way to exercise the real schedule
// (rather than a shortened stand-in) inside a unit test.
type backoffRecorder struct {
	mu sync.Mutex
	d  []time.Duration
}

func (r *backoffRecorder) record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.d = append(r.d, d)
}

func (r *backoffRecorder) durations() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.d...)
}

// captureBackoffs stubs BOTH backoff clocks: sleepFn (the per-file
// transient retry) and waitFn (the circuit breaker's pauses). Missing
// either one means a test really waits -- the breaker's schedule alone is
// ~35 minutes.
//
// The waitFn stub records the rung's TOTAL duration, so assertions can
// still check the schedule exactly, and fires a single tick so the
// onBackoff callback wiring is still exercised on this path.
func captureBackoffs(t *testing.T) *backoffRecorder {
	t.Helper()
	rec := &backoffRecorder{}
	origSleep, origWait := sleepFn, waitFn
	sleepFn = rec.record
	waitFn = func(ctx context.Context, total time.Duration, onTick func(remaining time.Duration)) bool {
		rec.record(total)
		onTick(total)
		return true
	}
	t.Cleanup(func() { sleepFn, waitFn = origSleep, origWait })
	return rec
}

// throttleBody is the real wording Google returns for the undocumented
// concurrent-write throttle the user hit in production.
const throttleBody = `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'"}}`

// batchCreateOKHandler is the standard fake batchCreate: every item
// succeeds, media item id derived from the upload token.
func batchCreateOKHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewMediaItems []struct {
			SimpleMediaItem struct {
				UploadToken string `json:"uploadToken"`
			} `json:"simpleMediaItem"`
		} `json:"newMediaItems"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	type resultItem struct {
		MediaItem struct {
			ID string `json:"id"`
		} `json:"mediaItem"`
	}
	var results []resultItem
	for _, item := range req.NewMediaItems {
		var ri resultItem
		ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
		results = append(results, ri)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
}

// TestUploader_Run_GenuineDailyQuotaWording_ButLowLocalUsage_IsReclassified
// covers the cross-validation layer directly, now that the classifier
// itself (retryx.ClassifyResponse) only accepts the explicit "per day"
// wording. This message IS Google's genuine daily-exhaustion phrasing, so
// it reaches reclassifyDailyQuota as DailyQuota -- and with the local
// counter at 1,843 (nowhere near the 10k/day ceiling), gpsync must still
// downgrade it to a throttle and keep going rather than abandoning the rest
// of the run.
//
// This is what the recalibrated threshold buys: the old bar demanded
// ~8,800 locally-counted requests before it would ever trust a DailyQuota
// classification, which became unreachable once the counter stopped
// counting raw byte-upload calls (one batchCreate now covers ~20 files) --
// so every genuine per-day 429 was silently downgraded and the reactive
// daily-quota stop never fired at all.
func TestUploader_Run_GenuineDailyQuotaWording_ButLowLocalUsage_IsReclassified(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0") // keep the test fast; the retry path is what's under test
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota metric 'requests' and limit 'requests per day'"}}`)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	captureBackoffs(t) // the retry now goes through the circuit breaker's 5s first rung

	db := openTestDB(t)
	mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 1843))

	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("real-content"))
	mustDB(t, db.EnsurePending("hash-lowusage", 12, "image/jpeg", p, nil))

	var dailyQuotaCalls int
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			// handleError's genuine daily-quota announcement is the only
			// one that reports a zero concurrency; a retry-style throttle
			// always reports the (>=1) limit it just shrank to. Backoff
			// seconds alone can't tell them apart here, since this test
			// deliberately serves Retry-After: 0 to stay fast.
			if newConcurrency == 0 {
				dailyQuotaCalls++
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1 -- a per-day 429 with local usage at 1843 must be downgraded to a throttle and retried", stats.Uploaded)
	}
	if u.quotaHitThisRun.Load() {
		t.Error("quotaHitThisRun must not be set when the local counter contradicts the daily-quota claim")
	}
	if dailyQuotaCalls != 0 {
		t.Errorf("dailyQuotaCalls = %d, want 0", dailyQuotaCalls)
	}
}

// TestReclassifyDailyQuota_ThresholdBoundary pins the recalibrated
// threshold from both sides, without any HTTP in the way: below it a
// DailyQuota claim is downgraded to a retryable throttle, at or above it
// Google's own claim is trusted and the run stops. It also confirms the
// other classifications pass through untouched -- reclassification must
// only ever soften DailyQuota, never re-label anything else.
func TestReclassifyDailyQuota_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		name string
		used int
		in   retryx.Kind
		want retryx.Kind
	}{
		{"far below the threshold", 100, retryx.DailyQuota, retryx.Throttle},
		{"just below the threshold", dailyQuotaTrustThreshold - 1, retryx.DailyQuota, retryx.Throttle},
		{"exactly at the threshold", dailyQuotaTrustThreshold, retryx.DailyQuota, retryx.DailyQuota},
		{"well above the threshold", 9000, retryx.DailyQuota, retryx.DailyQuota},
		{"throttle is left alone", 100, retryx.Throttle, retryx.Throttle},
		{"permanent is left alone", 100, retryx.Permanent, retryx.Permanent},
		{"transient is left alone", 9000, retryx.Transient, retryx.Transient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), tc.used))
			u := &Uploader{db: db, dailyQuota: quota.NewDailyQuota(db)}
			got := u.reclassifyDailyQuota(retryx.Classification{Kind: tc.in, Message: "m"})
			if got.Kind != tc.want {
				t.Errorf("with used=%d, %q reclassified to %q, want %q", tc.used, tc.in, got.Kind, tc.want)
			}
		})
	}
}

// TestUploader_OnDBError_ReportsFailedLedgerWrites proves failed ledger
// writes on the upload hot path are no longer discarded. The write that
// matters most is MarkUploaded: it runs AFTER Google already has the file's
// bytes, so losing it leaves the row `pending` and the next run silently
// re-uploads a file that is already safely stored -- wasted quota and
// bandwidth, with the real cause (disk full, corrupt DB) never reaching the
// user. A closed database stands in for any such failure.
func TestUploader_OnDBError_ReportsFailedLedgerWrites(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origBatchURL := batchCreateURL
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { batchCreateURL = origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("content-a"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p, nil))

	var mu sync.Mutex
	var reported []string
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onDBError: func(context string, err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, context+": "+err.Error())
		},
	}

	// Every ledger write from here on fails.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stats Stats
	u.batchCreateAndLink([]uploadSuccess{{row: statedb.Upload{SHA256: "hash-a", FirstSourcePath: p, Size: 9}, token: "content-a"}}, &stats)

	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 {
		t.Fatal("no onDBError callback fired for a ledger write against a closed database -- these errors are being discarded again")
	}
	found := false
	for _, r := range reported {
		if strings.Contains(r, p) && strings.Contains(r, "uploaded") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a report naming the file whose MarkUploaded failed, got %v", reported)
	}
}

// TestUploader_OnDBError_NilCallbackIsSafe: onDBError is optional (`gpsync`
// wires it, tests and any other caller may not), and a failing ledger write
// must never panic the run.
func TestUploader_OnDBError_NilCallbackIsSafe(t *testing.T) {
	db := openTestDB(t)
	u := &Uploader{db: db, onDBError: nil}
	u.dbErr("some context", fmt.Errorf("boom")) // must not panic
	u.dbErr("some context", nil)
}

// TestUploader_Run_ProgressCarriesTheFailureReason proves the fix for a
// live view that showed only THAT a file failed. A run where dozens of
// files fail back-to-back used to print a column of bare "failed" marks
// with no explanation anywhere on screen -- the reason lived only in the
// ledger, reachable after the fact via `gpsync log`. The reason recorded in
// the ledger must now also reach onProgress, from both failure paths: the
// per-file upload error and a batchCreate item rejection.
func TestUploader_Run_ProgressCarriesTheFailureReason(t *testing.T) {
	t.Run("upload failure", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT","message":"The file is corrupt and cannot be processed"}}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		origUploadURL := uploadURL
		uploadURL = srv.URL + "/v1/uploads"
		defer func() { uploadURL = origUploadURL }()

		db := openTestDB(t)
		dir := t.TempDir()
		p := writeFile(t, dir, "bad.jpg", []byte("corrupt"))
		mustDB(t, db.EnsurePending("hash-bad", 7, "image/jpeg", p, nil))

		var events []ProgressEvent
		u := &Uploader{
			client:      srv.Client(),
			db:          db,
			cfg:         config.Defaults(),
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(6),
			onProgress:  func(e ProgressEvent) { events = append(events, e) },
		}
		if _, err := u.Run(); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d progress events, want 1: %+v", len(events), events)
		}
		if events[0].LastOK {
			t.Fatal("expected a failure event")
		}
		if events[0].LastErrorMessage != "The file is corrupt and cannot be processed" {
			t.Errorf("LastErrorMessage = %q, want the API's own message -- a live view has nothing else to show the user", events[0].LastErrorMessage)
		}
		if events[0].LastErrorKind != string(retryx.Permanent) {
			t.Errorf("LastErrorKind = %q, want %q -- a dashboard badge needs the real classification to show FAILED rather than a generic fail", events[0].LastErrorKind, retryx.Permanent)
		}
		// And it must match exactly what went into the ledger.
		row, err := db.GetUpload("hash-bad")
		if err != nil {
			t.Fatal(err)
		}
		if row.LastErrorMessage.String != events[0].LastErrorMessage {
			t.Errorf("progress message %q != ledger message %q", events[0].LastErrorMessage, row.LastErrorMessage.String)
		}
	})

	t.Run("batchCreate item rejection", func(t *testing.T) {
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		})
		mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": []map[string]any{
				{"status": map[string]any{"message": "Failed: There was an error while trying to create this media item."}},
			}})
		})
		srv = httptest.NewServer(mux)
		defer srv.Close()

		origUploadURL, origBatchURL := uploadURL, batchCreateURL
		uploadURL = srv.URL + "/v1/uploads"
		batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
		defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

		db := openTestDB(t)
		dir := t.TempDir()
		p := writeFile(t, dir, "a.jpg", []byte("content-a"))
		mustDB(t, db.EnsurePending("hash-rejected", 9, "image/jpeg", p, nil))

		var events []ProgressEvent
		u := &Uploader{
			client:      srv.Client(),
			db:          db,
			cfg:         config.Defaults(),
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(6),
			onProgress:  func(e ProgressEvent) { events = append(events, e) },
		}
		if _, err := u.Run(); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].LastOK {
			t.Fatalf("expected exactly one failure event, got %+v", events)
		}
		if !strings.Contains(events[0].LastErrorMessage, "error while trying to create this media item") {
			t.Errorf("LastErrorMessage = %q, want batchCreate's own per-item status message", events[0].LastErrorMessage)
		}
		if events[0].LastErrorKind != string(retryx.Transient) {
			t.Errorf("LastErrorKind = %q, want %q -- there's no retryx.Classification for a per-item batchCreate rejection (no HTTP error to classify), and it lands failed_retryable, which \"transient\" describes correctly", events[0].LastErrorKind, retryx.Transient)
		}
	})
}

// TestUploader_Run_DailyQuotaProgressEvent_CarriesDailyQuotaKind proves the
// reactive 429-daily-quota path (handleError's own DailyQuota case) tags
// its progress event "daily_quota", not a generic failure -- the dashboard
// badge needs this to show QUOTA instead of FAILED.
func TestUploader_Run_DailyQuotaProgressEvent_CarriesDailyQuotaKind(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota metric 'requests' and limit 'requests per day'"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL := uploadURL
	uploadURL = srv.URL + "/v1/uploads"
	defer func() { uploadURL = origUploadURL }()

	db := openTestDB(t)
	// Local usage above dailyQuotaTrustThreshold, same reasoning as
	// TestUploader_Run_GenuineDailyQuota_StillRespected -- reclassifyDailyQuota
	// must trust the server's claim, not downgrade it to Transient.
	mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 9000))

	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("content-a"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p, nil))

	var events []ProgressEvent
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onProgress:  func(e ProgressEvent) { events = append(events, e) },
	}
	u.Run() // ErrRunAborted expected (genuine daily-quota exhaustion) -- irrelevant to this test

	if len(events) != 1 || events[0].LastOK {
		t.Fatalf("expected exactly one failure event, got %+v", events)
	}
	if events[0].LastErrorKind != string(retryx.DailyQuota) {
		t.Errorf("LastErrorKind = %q, want %q", events[0].LastErrorKind, retryx.DailyQuota)
	}
}

// TestUploader_MarkRemainingRetryable_ProgressEventCarriesThrottleOrCancelledKind
// proves markRemainingRetryable (the "the whole backoff schedule ran out,
// write this row off as failed_retryable" path) tags its progress event
// "throttle" normally, or "cancelled" when the caller explicitly benched
// the row early (a Retry Now click) -- direct unit test since exercising
// this through a full Run() would mean re-simulating the entire backoff
// schedule, which is already covered elsewhere (see
// TestThrottleCircuitBreakerSchedule_FirstRungIsLongEnoughToBeWorthTaking and
// neighbors) and isn't what this is trying to prove.
func TestUploader_MarkRemainingRetryable_ProgressEventCarriesThrottleOrCancelledKind(t *testing.T) {
	cases := []struct {
		name      string
		cancelled bool
		wantKind  retryx.Kind
	}{
		{"schedule exhausted", false, retryx.Throttle},
		{"cancelled via Retry Now", true, retryx.Cancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			mustDB(t, db.EnsurePending("hash-benched", 10, "image/jpeg", "/lib/benched.jpg", nil))
			row, err := db.GetUpload("hash-benched")
			if err != nil || row == nil {
				t.Fatal(err)
			}

			var events []ProgressEvent
			u := &Uploader{db: db, onProgress: func(e ProgressEvent) { events = append(events, e) }}
			tally := &runTally{total: 1}

			u.markRemainingRetryable([]statedb.Upload{*row}, tally, "schedule exhausted", tc.cancelled)

			if len(events) != 1 {
				t.Fatalf("got %d progress events, want 1: %+v", len(events), events)
			}
			if events[0].LastErrorKind != string(tc.wantKind) {
				t.Errorf("LastErrorKind = %q, want %q", events[0].LastErrorKind, tc.wantKind)
			}
			if events[0].LastCancelled != tc.cancelled {
				t.Errorf("LastCancelled = %v, want %v", events[0].LastCancelled, tc.cancelled)
			}
		})
	}
}

// TestUploader_Run_SuccessEventsCarryNoErrorMessage: a successful file must
// never carry stale error text into a live view.
func TestUploader_Run_SuccessEventsCarryNoErrorMessage(t *testing.T) {
	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("content-a"))
	mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p, nil))

	var events []ProgressEvent
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onProgress:  func(e ProgressEvent) { events = append(events, e) },
	}
	if _, err := u.Run(); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.LastOK && e.LastErrorMessage != "" {
			t.Errorf("successful file reported an error message: %+v", e)
		}
	}
}

// TestUploader_Run_WorkersKeepDispatchingDuringABatchFlush proves the fix
// for a pipeline that collapsed to near-zero parallelism on every batch
// commit. Each worker sends its result BEFORE its `defer func() { <-sem }()`
// runs (defers fire only once the send statement has returned), so with an
// unbuffered results channel a finished worker sat blocked on the send while
// still holding its concurrency slot. The drain loop is unavailable for the
// whole of flush() -- three sequential network round-trips -- so every
// worker that finished during a flush froze with it, and no new file could
// start. Buffering the channel to the concurrency limit lets the hand-off
// complete immediately and frees the slot right away.
func TestUploader_Run_WorkersKeepDispatchingDuringABatchFlush(t *testing.T) {
	const concurrency = 4
	var mu sync.Mutex
	inFlush := false
	startsDuringFlush := 0
	// Signalled the first time a file starts uploading while a batchCreate
	// flush is still in progress. The flush waits on this rather than
	// sleeping a fixed window: an assertion about an observed interleaving is
	// otherwise only as reliable as the scheduler under whatever else the
	// machine is doing, and this one failed exactly once under -race with the
	// full suite running in parallel, while passing in isolation.
	startedDuringFlush := make(chan struct{}, 1)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if inFlush {
			startsDuringFlush++
			select {
			case startedDuringFlush <- struct{}{}:
			default:
			}
		}
		mu.Unlock()
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlush = true
		mu.Unlock()
		// Hold the flush open until a file actually starts uploading, rather
		// than sleeping a fixed window and hoping one lands inside it.
		//
		// Deliberately ONE start, not `concurrency` of them: a stricter
		// threshold was tried and reverted, because under -race on a loaded
		// runner it reports zero and fails a pipeline that is working. The
		// honest scope of this test is "dispatch is not frozen for the whole
		// of every flush"; it is not a proof that throughput is maintained,
		// and making the unbuffered-results regression fail it has never
		// been demonstrated.
		select {
		case <-startedDuringFlush:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		inFlush = false
		mu.Unlock()
		batchCreateOKHandler(w, r)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	// Comfortably more than batchFlushCount, so a flush happens with plenty
	// of files still waiting to be dispatched.
	for i := 0; i < batchFlushCount*2+10; i++ {
		name := fmt.Sprintf("f%03d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%03d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: concurrency},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, concurrency),
	}
	if _, err := u.Run(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := startsDuringFlush
	mu.Unlock()
	if got == 0 {
		t.Error("no file started uploading while a batchCreate flush was in progress -- dispatch is frozen for the whole of every flush")
	}
}

// throttlingServer is a fake API that 429s with real "concurrent write
// request" throttle wording for the first throttleUntil upload-start
// requests, then behaves normally. It counts start requests so a test can
// tell how many dispatch rounds actually reached the network.
type throttlingServer struct {
	mu            sync.Mutex
	starts        int
	throttleAll   bool
	throttleFirst int
}

func (ts *throttlingServer) startCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.starts
}

func (ts *throttlingServer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.starts++
		n := ts.starts
		throttle := ts.throttleAll || n <= ts.throttleFirst
		ts.mu.Unlock()
		if throttle {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, throttleBody)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	t.Cleanup(func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL })
	return srv
}

// TestUploader_Run_CircuitBreaker_FollowsTheFullScheduleThenGivesUp proves
// the core of the run-wide circuit breaker against a server that never lets
// up.
//
// The design it replaced retried each throttled file independently, up to a
// per-file budget, while every OTHER file kept dispatching at full
// concurrency the whole time -- so the aggregate request rate against
// Google barely dropped and the "concurrent write request" throttle simply
// never cleared. gpsync was keeping its own rate limit alive.
//
// Now the first throttle stops the entire pipeline, and the run pauses on a
// fixed escalating ladder (5s, 30s, 1m, 3m, 5m, 10m, 15m). This asserts the
// real schedule -- exactly, in order -- by driving the sleep clock rather
// than shortening it, and that after the last rung is spent the run gives
// up instead of looping forever, leaving every unfinished file
// failed_retryable for the next invocation.
func TestUploader_Run_CircuitBreaker_FollowsTheFullScheduleThenGivesUp(t *testing.T) {
	rec := captureBackoffs(t)
	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	// 8, not 3 -- enough files that at concurrency 2, per-file strikes
	// spread thin enough for NONE to reach maxThrottleStrikes (3) within
	// len(throttleCircuitBreakerSchedule) passes, so the run-wide breaker
	// this test exists to check is what actually gives up (verified
	// empirically: 3 files let one bench out via individual strikes
	// before the real 9-rung schedule fully exhausts, exiting cleanly
	// instead of via the abort this test asserts on).
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	var throttleAnnouncements int
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 2},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(2),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			if backoffSeconds > 0 {
				throttleAnnouncements++
			}
		},
	}

	stats, err := u.Run()

	if !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted -- an unclearable throttle must tell the caller to stop the whole invocation", err)
	}
	var abort *AbortError
	if !errors.As(err, &abort) {
		t.Fatalf("error %v is not an *AbortError", err)
	}
	if abort.Reason != AbortThrottleBackoffExhausted {
		t.Errorf("abort reason = %q, want %q", abort.Reason, AbortThrottleBackoffExhausted)
	}

	// The exact real schedule, in order, once each.
	got := rec.durations()
	if len(got) != len(throttleCircuitBreakerSchedule) {
		t.Fatalf("slept %d times (%v), want exactly one pause per schedule rung (%v)", len(got), got, throttleCircuitBreakerSchedule)
	}
	for i, want := range throttleCircuitBreakerSchedule {
		if got[i] != want {
			t.Errorf("backoff %d = %v, want %v (full sequence: %v)", i+1, got[i], want, got)
		}
	}
	var wantTotal time.Duration
	for _, d := range throttleCircuitBreakerSchedule {
		wantTotal += d
	}
	if abort.Waited != wantTotal {
		t.Errorf("reported total wait = %v, want the schedule's sum %v", abort.Waited, wantTotal)
	}
	if abort.Steps != len(throttleCircuitBreakerSchedule) {
		t.Errorf("reported steps = %d, want %d", abort.Steps, len(throttleCircuitBreakerSchedule))
	}
	if throttleAnnouncements != len(throttleCircuitBreakerSchedule) {
		t.Errorf("onThrottle fired %d times, want %d -- every pause must be visible, not a silent freeze", throttleAnnouncements, len(throttleCircuitBreakerSchedule))
	}

	// Nothing uploaded, and everything left is failed_retryable so the next
	// invocation's RequeueRetryable picks it straight back up.
	if stats.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0", stats.Uploaded)
	}
	if stats.FailedRetryable != 8 {
		t.Errorf("FailedRetryable = %d, want 8", stats.FailedRetryable)
	}
	failures, err := db.ListFailures(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 8 {
		t.Fatalf("ledger has %d failures, want 8", len(failures))
	}
	for _, f := range failures {
		if f.Status != "failed_retryable" {
			t.Errorf("%s status = %q, want failed_retryable", f.FirstSourcePath, f.Status)
		}
		if f.LastErrorCode.String != "THROTTLE_BACKOFF_EXHAUSTED" {
			t.Errorf("%s error code = %q, want THROTTLE_BACKOFF_EXHAUSTED", f.FirstSourcePath, f.LastErrorCode.String)
		}
	}
}

// TestUploader_Run_InheritedExhaustedBreaker_WaitsOnceBeforeGivingUp is the
// fix for a real, serious report: "at some point the throttle/backoff just
// cease to work" -- a screenshot showing a wall of files each individually
// marked FAIL with "gave up after 0s of throttle backoff", one after
// another with no visible pause between them.
//
// Root cause: gpsync-tray shares ONE *CircuitBreakerState across every
// folder's own Uploader.Run() call (see
// TestUploader_CircuitBreaker_SharedAcrossRunCalls_ContinuesInsteadOfResetting).
// Once one folder's Run() call genuinely walks the full ladder and gives
// up (the scenario above, waited > 0), the breaker's rung is left sitting
// at the schedule's ceiling. The VERY NEXT folder's Run() call starts its
// own `waited` at zero, and if its first real dispatch attempt is throttled
// again (very likely -- Google's real limit hasn't had any time to
// recover), the old code checked "is the schedule exhausted" BEFORE ever
// waiting, found it already was, and aborted on the spot -- zero wait,
// zero rest for Google's endpoint, and this repeats for every subsequent
// folder cycle for as long as the real throttle persists: exactly what the
// circuit breaker exists to prevent, and exactly why the backoff appeared
// to "stop working" (no pause ever visibly happened again).
//
// A Run() call that inherits an already-exhausted breaker without ever
// climbing a rung itself (waited==0 at the moment it discovers this) must
// now pay for one real rest at the final rung before giving up.
func TestUploader_Run_InheritedExhaustedBreaker_WaitsOnceBeforeGivingUp(t *testing.T) {
	rec := captureBackoffs(t)
	breaker := NewCircuitBreakerState()

	// 8 files at concurrency 2, exactly like
	// TestUploader_Run_CircuitBreaker_FollowsTheFullScheduleThenGivesUp --
	// a single file at low concurrency hits maxThrottleStrikes (benched,
	// clean EndReasonCompleted) well before the shared rung ever reaches
	// the schedule's ceiling, since every one of its throttles counts as
	// a strike against that one file. Enough files (verified empirically
	// against the current 9-rung schedule) spreads per-file strikes thin
	// enough that none reach maxThrottleStrikes before the schedule fully
	// exhausts, so the RUN-WIDE breaker -- not any single file's strike
	// count -- is what actually gives up here, which is the mechanism
	// this test exists to exercise.
	runOneFolder := func(t *testing.T) (Stats, error) {
		t.Helper()
		ts := &throttlingServer{throttleAll: true}
		ts.serve(t)

		db := openTestDB(t)
		dir := t.TempDir()
		for i := 0; i < 8; i++ {
			name := fmt.Sprintf("f%d.jpg", i)
			p := writeFile(t, dir, name, []byte(name))
			mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%s-%d", dir, i), int64(len(name)), "image/jpeg", p, nil))
		}

		u := &Uploader{
			client:      http.DefaultClient,
			db:          db,
			cfg:         config.Config{Concurrency: 2},
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(2),
			breaker:     breaker,
		}
		return u.Run()
	}

	// Folder one: never lets up, walks the whole ladder, gives up --
	// breaker.rung is now at the ceiling. Same shape as
	// TestUploader_Run_CircuitBreaker_FollowsTheFullScheduleThenGivesUp.
	_, err1 := runOneFolder(t)
	var abort1 *AbortError
	if !errors.As(err1, &abort1) || abort1.Reason != AbortThrottleBackoffExhausted {
		t.Fatalf("folder one error = %v, want *AbortError{Reason: AbortThrottleBackoffExhausted}", err1)
	}

	// Folder two starts immediately after, inheriting the already-maxed
	// breaker, and is ALSO throttled on its very first request -- exactly
	// the bug scenario. It must NOT abort with zero additional wait.
	_, err2 := runOneFolder(t)
	var abort2 *AbortError
	if !errors.As(err2, &abort2) || abort2.Reason != AbortThrottleBackoffExhausted {
		t.Fatalf("folder two error = %v, want *AbortError{Reason: AbortThrottleBackoffExhausted}", err2)
	}

	finalRung := throttleCircuitBreakerSchedule[len(throttleCircuitBreakerSchedule)-1]
	if abort2.Waited != finalRung {
		t.Errorf("folder two Waited = %v, want exactly one final-rung wait (%v) -- it must pay for a real rest instead of giving up instantly", abort2.Waited, finalRung)
	}

	got := rec.durations()
	wantCount := len(throttleCircuitBreakerSchedule) + 1 // folder one's full ladder, plus folder two's one rest
	if len(got) != wantCount {
		t.Fatalf("slept %d times %v, want %d (folder one's %d rungs + folder two's one final-rung wait)", len(got), got, wantCount, len(throttleCircuitBreakerSchedule))
	}
	if last := got[len(got)-1]; last != finalRung {
		t.Errorf("folder two's wait = %v, want the final rung's duration %v", last, finalRung)
	}
}

// TestUploader_Run_CircuitBreaker_RampsDownAfterSustainedRecovery is the
// fix for a real complaint: two throttles back to back (5s, then 30s rung)
// climb the ladder as expected, but then a long stretch of clean, fully
// successful dispatch happens before a third throttle hits. Without
// ramp-down, that third throttle would cost 1m (the next rung up from
// where the second one left off) no matter how well things had been going
// in between -- strictly worse than the last pause purely because a
// throttle happened again eventually, which on a large enough folder it
// always will. With ramp-down, decayUploadsPerRung*2 (20) real successful
// uploads is enough to pay back both prior escalations, so the third
// throttle costs the FIRST rung again (5s), not a worse one. Real, direct
// request behind counting UPLOADS rather than elapsed time (an earlier
// version of this test used a fake clock instead): "after x successful
// uploads, i would expect to ease on the ramp-up.. (basically start the
// ramp down) - so 10 successful uploads - one rung down."
func TestUploader_Run_CircuitBreaker_RampsDownAfterSustainedRecovery(t *testing.T) {
	rec := captureBackoffs(t)

	var mu sync.Mutex
	n := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		this := n
		mu.Unlock()

		// Requests 1 and 2 (the first attempt of each of the first two
		// passes) throttle, forcing two escalations. Requests 3-22 (20
		// files -- exactly decayUploadsPerRung*2) succeed cleanly, paying
		// back both escalations. Request 23 (the last file) throttles a
		// third time.
		throttle := this == 1 || this == 2 || this == 23
		if throttle {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, throttleBody)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	const totalFiles = 23
	for i := 0; i < totalFiles; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1}, // deterministic request ordering
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run() = %v, want a clean finish -- every file resolves after the third (final) throttle clears", err)
	}
	if stats.Uploaded != totalFiles {
		t.Errorf("Uploaded = %d, want %d", stats.Uploaded, totalFiles)
	}

	want := []time.Duration{
		throttleCircuitBreakerSchedule[0], // 5s -- first throttle
		throttleCircuitBreakerSchedule[1], // 30s -- second throttle, no successes banked yet
		throttleCircuitBreakerSchedule[0], // 5s again -- ramped back down after 20 real successes
	}
	got := rec.durations()
	if len(got) != len(want) {
		t.Fatalf("slept %d times %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff %d = %v, want %v (full sequence: %v, want: %v)", i+1, got[i], want[i], got, want)
		}
	}
}

// TestUploader_Run_CircuitBreaker_NoProgressNeverDecays is the fix for a
// real report: a SUSTAINED throttle (nothing ever succeeds) got stuck
// retrying at the 5s rung forever instead of ever escalating. Decay is now
// driven by counting real successful uploads (see decayedRung's own doc
// comment for why this replaced an earlier elapsed-wall-clock-time
// measurement), so a run where NOTHING ever succeeds structurally can never
// bank anything toward a decay -- this locks that invariant in: the
// schedule must still escalate through every rung in order, never
// oscillating back down just because throttled attempts kept happening.
// TestThrottleCircuitBreakerSchedule_FirstRungIsLongEnoughToBeWorthTaking
// guards the schedule's floor against a future change reintroducing a
// fast first retry.
//
// The 30s floor came from Google's own documented guidance ("On 429, wait
// at least 30 seconds before retrying... Fast retries on a 429 count as
// more concurrent writes and dig the hole deeper"). The floor is now 5m,
// raised on MEASURED evidence from this project's own throttle_events log
// -- see throttleCircuitBreakerSchedule's doc comment for the full table.
// The short version: across 261 events, a 30s wait bought 0.9 files on
// average and bought NOTHING 89% of the time; a 1m wait bought one single
// file across 40 events. Those rungs weren't merely slow, they were
// retries that could not work, each one provoking the very throttle it
// was waiting out.
//
// Asserted as ">= 30s" rather than "== 5m" deliberately: the exact value
// is expected to keep moving as more data arrives, but dropping back
// under Google's documented minimum should never happen silently.
func TestThrottleCircuitBreakerSchedule_FirstRungIsLongEnoughToBeWorthTaking(t *testing.T) {
	if len(throttleCircuitBreakerSchedule) == 0 {
		t.Fatal("throttleCircuitBreakerSchedule is empty")
	}
	if got := throttleCircuitBreakerSchedule[0]; got < 30*time.Second {
		t.Errorf("first rung = %v, want at least 30s (Google's own documented minimum retry wait for a 429 on this endpoint)", got)
	}
	// The ladder must still climb -- a flat or descending schedule would
	// mean escalation buys nothing at all.
	for i := 1; i < len(throttleCircuitBreakerSchedule); i++ {
		if throttleCircuitBreakerSchedule[i] <= throttleCircuitBreakerSchedule[i-1] {
			t.Errorf("rung %d (%v) does not exceed rung %d (%v) -- the schedule must be strictly increasing",
				i, throttleCircuitBreakerSchedule[i], i-1, throttleCircuitBreakerSchedule[i-1])
		}
	}
	// The ceiling is where the throughput actually comes from (65% of all
	// waiting AND ~three quarters of all successful uploads landed after a
	// 1h wait), so it must not be quietly shortened.
	if got := throttleCircuitBreakerSchedule[len(throttleCircuitBreakerSchedule)-1]; got < time.Hour {
		t.Errorf("final rung = %v, want at least 1h -- measured as the only rung that reliably clears this account's throttle", got)
	}
}

// TestBatchFlushInterval_DefaultIsThirtySeconds locks in the production
// default -- see batchFlushInterval's own doc comment for the real
// research behind why this isn't 1 second anymore: upload tokens stay
// valid 24 hours, so there's no protocol-level urgency to flush before
// batchFlushCount (50) is reached, and every premature flush is a smaller,
// less quota-efficient batchCreate call than necessary.
func TestBatchFlushInterval_DefaultIsThirtySeconds(t *testing.T) {
	if batchFlushInterval != 30*time.Second {
		t.Errorf("batchFlushInterval = %v, want 30s", batchFlushInterval)
	}
}

func TestUploader_Run_CircuitBreaker_NoProgressNeverDecays(t *testing.T) {
	rec := captureBackoffs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, throttleBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL := uploadURL
	uploadURL = srv.URL + "/v1/uploads"
	defer func() { uploadURL = origUploadURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	// 8, not 1: with maxThrottleStrikes capping any ONE file at 3 throttle
	// hits before it's benched (see that const's doc comment), a single
	// always-throttling file can't survive to see all 9 schedule rungs --
	// it gets benched early. With concurrency 1, dispatch round-robins one
	// file per pass (undispatchedRows always go first next queue), and 8
	// files spreads strikes thin enough (empirically verified, same
	// margin as TestUploader_Run_CircuitBreaker_FollowsTheFullScheduleThenGivesUp)
	// that none get benched before the full 9-rung schedule is exercised.
	for i := 0; i < 8; i++ {
		p := writeFile(t, dir, fmt.Sprintf("f%d.jpg", i), []byte(fmt.Sprintf("f%d", i)))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), 2, "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
	}

	_, err := u.Run()
	if !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run() error = %v, want ErrRunAborted", err)
	}

	got := rec.durations()
	if len(got) != len(throttleCircuitBreakerSchedule) {
		t.Fatalf("slept %d times %v, want exactly one pause per schedule rung %v -- decay must never fire on elapsed time alone without any real progress",
			len(got), got, throttleCircuitBreakerSchedule)
	}
	for i, want := range throttleCircuitBreakerSchedule {
		if got[i] != want {
			t.Errorf("backoff %d = %v, want %v (full sequence: %v)", i+1, got[i], want, got)
		}
	}
}

// TestUploader_Run_ContextCancelledDuringBackoff_StopsEarlyWithoutEscalating
// is the fix for a real report: gpsync-tray's Quit "didn't work" -- the user
// had to kill the process -- because a Quit/Pause click called
// watchController.Stop(), which waited for the in-progress cycle's
// Uploader.Run() to return, and Run() had no way to be interrupted mid
// circuit-breaker pause (up to the schedule's 15m final rung). Cancelling
// u.ctx during the wait must make Run() return almost immediately, leave
// the held file failed_retryable (picked back up by the next Run()), and
// must NOT escalate the shared breaker -- this pass never got to actually
// confirm the throttle by waiting it out, so it isn't real evidence either
// way.
func TestUploader_Run_ContextCancelledDuringBackoff_StopsEarlyWithoutEscalating(t *testing.T) {
	origTick := backoffTickInterval
	backoffTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { backoffTickInterval = origTick })

	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	ctx, cancel := context.WithCancel(context.Background())
	breaker := NewCircuitBreakerState()

	var backoffTicks int
	u := &Uploader{
		ctx:         ctx,
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		breaker:     breaker,
		onBackoff: func(s BackoffStatus) {
			if s.Active {
				backoffTicks++
				if backoffTicks == 2 {
					cancel()
				}
			}
		},
	}

	done := make(chan struct{})
	var stats Stats
	var runErr error
	go func() {
		stats, runErr = u.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after ctx was cancelled -- the backoff wait is not interruptible")
	}

	var abort *AbortError
	if !errors.As(runErr, &abort) || abort.Reason != AbortInterrupted {
		t.Fatalf("Run() error = %v, want an *AbortError{Reason: AbortInterrupted}", runErr)
	}
	if stats.FailedRetryable != 0 {
		t.Errorf("FailedRetryable = %d, want 0 -- interrupted mid-wait, nothing was ever actually confirmed as failed", stats.FailedRetryable)
	}

	breaker.mu.Lock()
	gotRung := breaker.rung
	breaker.mu.Unlock()
	if gotRung != 0 {
		t.Errorf("breaker.rung = %d, want 0 -- an interrupted wait never confirmed the throttle, so it must not escalate", gotRung)
	}

	row, gerr := db.GetUpload("hash-a")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.Status != "pending" {
		t.Errorf("row status = %q, want pending -- an interrupted backoff wait must leave the file untouched, not spam it into failed_retryable (a real report: pausing mid-throttle under a global sync strategy dumped tens of thousands of files into failed_retryable at once)", row.Status)
	}
}

// TestUploader_Run_ContextCancelledMidDispatch_LeavesUndispatchedFilesPending
// is gpsync-tray's "graceful Quit/Pause": don't push more uploads, just finish
// what's already transferring, then stop. With concurrency 1 the dispatcher
// can only be mid-file-1 when cancel fires (file 2's dispatch is parked in
// dispatchPass's semaphore-wait poll loop, which rechecks ctx.Err() every
// 50ms) -- so file 1 must complete normally and files 2-5 must never be
// attempted at all, left exactly as they were (`pending`, no ledger write) --
// same outcome as the backoff-wait interrupt case above, which leaves its
// remainder `pending` too even though those files DID get a real throttle
// response this pass: neither case ever actually confirmed a failure, one
// was just paused mid-dispatch and the other mid-wait.
func TestUploader_Run_ContextCancelledMidDispatch_LeavesUndispatchedFilesPending(t *testing.T) {
	ts := &throttlingServer{} // throttleFirst/throttleAll both zero-value: every request succeeds
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	var hashes []string
	for i := 0; i < 5; i++ {
		p := writeFile(t, dir, fmt.Sprintf("f%d.jpg", i), []byte(fmt.Sprintf("content-%d", i)))
		hash := fmt.Sprintf("hash-%d", i)
		mustDB(t, db.EnsurePending(hash, 8, "image/jpeg", p, nil))
		hashes = append(hashes, hash)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var cancelOnce sync.Once
	u := &Uploader{
		ctx:         ctx,
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		onBytes:     func(ByteProgressEvent) { cancelOnce.Do(cancel) },
	}

	stats, runErr := u.Run()

	var abort *AbortError
	if !errors.As(runErr, &abort) || abort.Reason != AbortInterrupted {
		t.Fatalf("Run() error = %v, want *AbortError{Reason: AbortInterrupted}", runErr)
	}
	if abort.Steps != 0 {
		t.Errorf("Steps = %d, want 0 -- mid-dispatch interrupt, not a backoff-wait one", abort.Steps)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 -- the file already in flight when ctx was cancelled must still finish", stats.Uploaded)
	}

	pendingLeft, uploaded := 0, 0
	for _, h := range hashes {
		row, gerr := db.GetUpload(h)
		if gerr != nil {
			t.Fatal(gerr)
		}
		switch row.Status {
		case "pending":
			pendingLeft++
		case "uploaded":
			uploaded++
		default:
			t.Errorf("row %s status = %q, want pending or uploaded -- an undispatched file must never be marked failed", h, row.Status)
		}
	}
	if uploaded != 1 {
		t.Errorf("uploaded rows = %d, want 1", uploaded)
	}
	if pendingLeft != 4 {
		t.Errorf("pending rows left = %d, want 4 -- untouched, since they were never even attempted", pendingLeft)
	}
}

// TestUploader_CircuitBreaker_SharedAcrossRunCalls_ContinuesInsteadOfResetting
// is the fix for a real report: gpsync watch draining a large pending backlog
// while genuinely still throttled looked like "retrying every 5s forever"
// -- because each new folder got its own fresh Uploader (and, before this
// fix, its own fresh rung state local to that one Run() call). A shared
// *CircuitBreakerState across two separate Uploader instances (standing in
// for two folders processed back to back, exactly what gpsync watch's backlog
// drain and gpsync sync's folder loop both do) must carry the rung forward:
// folder two's first throttle should escalate from wherever folder one
// left off, not start over at rung 0.
func TestUploader_CircuitBreaker_SharedAcrossRunCalls_ContinuesInsteadOfResetting(t *testing.T) {
	rec := captureBackoffs(t)
	breaker := NewCircuitBreakerState()

	runOneFolder := func(t *testing.T) (Stats, error) {
		t.Helper()
		ts := &throttlingServer{throttleFirst: 1}
		ts.serve(t)

		db := openTestDB(t)
		dir := t.TempDir()
		p := writeFile(t, dir, "photo.jpg", []byte("content"))
		mustDB(t, db.EnsurePending("hash-"+dir, 7, "image/jpeg", p, nil))

		u := &Uploader{
			client:      http.DefaultClient,
			db:          db,
			cfg:         config.Config{Concurrency: 1},
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(1),
			breaker:     breaker,
		}
		return u.Run()
	}

	// Folder one: one throttle, then the retry succeeds -- a clean finish,
	// exactly the common case of a folder that hit the wall once and
	// recovered on its own next pass. Breaker is now at rung 1.
	stats1, err1 := runOneFolder(t)
	if err1 != nil {
		t.Fatalf("folder one Run() = %v, want a clean finish", err1)
	}
	if stats1.Uploaded != 1 {
		t.Errorf("folder one Uploaded = %d, want 1", stats1.Uploaded)
	}

	// Folder two starts immediately after (no real time elapsed, so no
	// ramp-down decay applies) and is ALSO throttled on its very first
	// request. With the breaker shared, this must escalate to rung 2 (30s)
	// -- continuing from folder one -- not reset to rung 0 (5s) as if
	// nothing had just happened.
	stats2, err2 := runOneFolder(t)
	if err2 != nil {
		t.Fatalf("folder two Run() = %v, want a clean finish", err2)
	}
	if stats2.Uploaded != 1 {
		t.Errorf("folder two Uploaded = %d, want 1", stats2.Uploaded)
	}

	want := []time.Duration{
		throttleCircuitBreakerSchedule[0], // folder one's only throttle: 5s
		throttleCircuitBreakerSchedule[1], // folder two's first throttle: 30s, NOT 5s again
	}
	got := rec.durations()
	if len(got) != len(want) {
		t.Fatalf("slept %d times %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff %d = %v, want %v (full sequence: %v, want: %v) -- the breaker's rung must carry across folders, not reset",
				i+1, got[i], want[i], got, want)
		}
	}
}

// countingIdleCloser wraps a real RoundTripper so a test can prove
// CloseIdleConnections() actually gets called, not just that it compiles.
type countingIdleCloser struct {
	http.RoundTripper
	mu     sync.Mutex
	closes int
}

func (c *countingIdleCloser) CloseIdleConnections() {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
}

func (c *countingIdleCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// TestUploader_Run_CircuitBreaker_ClosesIdleConnectionsAfterEachWait proves
// the fix for stale pooled connections surviving a multi-minute pause: a
// connection idle through a 15-minute rung can easily have been dropped by
// Google's own load balancer already, and reusing it looks identical to a
// hung server -- the client writes into it successfully and then just
// never gets a response, burning the full ResponseHeaderTimeout instead of
// failing fast. u.client.CloseIdleConnections() must fire once per rung
// (after the wait, right before dispatch resumes), and NOT fire at all on
// a run that never throttles.
func TestUploader_Run_CircuitBreaker_ClosesIdleConnectionsAfterEachWait(t *testing.T) {
	captureBackoffs(t)
	ts := &throttlingServer{throttleFirst: 2} // two throttles -> two rungs -> two waits
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	closer := &countingIdleCloser{RoundTripper: http.DefaultTransport}
	u := &Uploader{
		client:      &http.Client{Transport: closer},
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
	}

	if _, err := u.Run(); err != nil {
		t.Fatalf("Run() = %v, want a clean finish once the throttle clears", err)
	}
	if got := closer.count(); got != 2 {
		t.Errorf("CloseIdleConnections called %d times, want 2 (one per rung waited)", got)
	}
}

// TestUploader_Run_CircuitBreaker_StopsDispatchingImmediatelyOnTheFirstThrottle
// proves step 2 of the design: the moment ANY file throttles, no NEW file
// is sent. With a large backlog and a server that throttles from the very
// first request, a per-file-retry design would have marched through the
// whole backlog (every file failing on its own and retrying on its own);
// the breaker must instead stop after roughly the handful of requests
// already in flight.
func TestUploader_Run_CircuitBreaker_StopsDispatchingImmediatelyOnTheFirstThrottle(t *testing.T) {
	captureBackoffs(t)
	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	const backlog = 60
	const concurrency = 3

	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < backlog; i++ {
		name := fmt.Sprintf("f%03d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%03d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: concurrency},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(concurrency),
	}
	if _, err := u.Run(); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted", err)
	}

	// Eight throttling rounds (one per rung, plus the final one that
	// exhausts the schedule), each allowed at most the in-flight window.
	// Anything close to backlog per round means dispatch never stopped.
	rounds := len(throttleCircuitBreakerSchedule) + 1
	maxExpected := rounds * concurrency
	if got := ts.startCount(); got > maxExpected {
		t.Errorf("server saw %d upload-start requests across %d throttled rounds, want at most %d (~%d in flight per round) -- the dispatcher kept feeding new files into a rate limit that was already complaining, which is exactly what keeps the throttle alive",
			got, rounds, maxExpected, concurrency)
	}
	if got := ts.startCount(); got < rounds {
		t.Errorf("server saw only %d requests, want at least one per round (%d) -- the breaker must actually retry after each pause", got, rounds)
	}
}

// TestUploader_Run_CircuitBreaker_ResetsAndResumesOnceTheThrottleClears
// proves the recovery half: after the pause, the files set aside are
// retried (they were never marked failed), the rest of the backlog resumes
// dispatching normally, and the run completes successfully with no abort
// signal at all. Only the rungs actually needed are ever slept.
func TestUploader_Run_CircuitBreaker_ResetsAndResumesOnceTheThrottleClears(t *testing.T) {
	rec := captureBackoffs(t)
	// Throttle exactly ONE request, then serve normally.
	//
	// It must be exactly one, because the mock counts REQUESTS while the
	// assertion below counts PASSES, and those two only coincide if every
	// throttled request happens to land in the same pass. Whether request
	// #2 shares pass 1 with request #1 is a scheduling race -- the
	// dispatcher has to loop around and dispatch it before request #1's
	// worker completes its round-trip and trips the breaker. Throttling a
	// second request therefore made this test's expected backoff count
	// depend on who won that race (one pause if they shared a pass, two if
	// request #2 slipped into the next one). With a single throttled
	// request the grouping cannot matter: there is one trip and one pause
	// however the passes fall.
	ts := &throttlingServer{throttleFirst: 1}
	ts.serve(t)

	const files = 8
	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	var deferredEvents, completedEvents int
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 2},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 2),
		onProgress: func(e ProgressEvent) {
			if e.LastFile == "" {
				return
			}
			if e.LastDeferred {
				deferredEvents++
			} else {
				completedEvents++
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil -- a throttle that clears must not abort the run", err)
	}
	if stats.Uploaded != files {
		t.Fatalf("Uploaded = %d, want %d -- every file, including the ones set aside by the breaker, must be retried after the pause", stats.Uploaded, files)
	}
	if stats.FailedRetryable != 0 || stats.FailedPermanent != 0 {
		t.Errorf("unexpected failures: %+v -- a throttled file is held for retry, never recorded as failed", stats)
	}

	if got := rec.durations(); len(got) != 1 || got[0] != throttleCircuitBreakerSchedule[0] {
		t.Errorf("backoffs = %v, want exactly one pause at the first rung (%v) -- the ladder must only climb while throttles keep coming", got, throttleCircuitBreakerSchedule[0])
	}
	if deferredEvents == 0 {
		t.Error("expected at least one deferred progress event for the throttled file(s)")
	}
	// Deferred files must not be double-counted: exactly one completion
	// event per file, however many times it was held and retried.
	if completedEvents != files {
		t.Errorf("completion events = %d, want %d (one per file) -- a held file must not also count as done", completedEvents, files)
	}

	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["uploaded"] != files {
		t.Errorf("ledger counts = %+v, want %d uploaded", counts, files)
	}
}

// TestUploader_Run_CircuitBreaker_LetsInFlightRequestsFinish proves the
// deliberate restraint in step 2: a trip stops NEW dispatch, it does not
// cancel work already in progress. A request that was already on the wire
// when the breaker tripped still gets its result processed normally -- here,
// a file that succeeds concurrently with the throttle must end up uploaded,
// not discarded or re-queued.
func TestUploader_Run_CircuitBreaker_LetsInFlightRequestsFinish(t *testing.T) {
	captureBackoffs(t)

	var mu sync.Mutex
	throttleNext := true
	activeTransfers := 0
	siblingsInFlightAtTrip := -1

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		throttle := throttleNext
		throttleNext = false // exactly one throttle, ever
		mu.Unlock()
		if throttle {
			// Give the sibling requests time to actually be in flight
			// alongside this one before the breaker trips.
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			siblingsInFlightAtTrip = activeTransfers
			mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, throttleBody)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		activeTransfers++
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		time.Sleep(150 * time.Millisecond) // still transferring when the breaker trips
		mu.Lock()
		activeTransfers--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 4},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 4),
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if stats.Uploaded != 4 {
		t.Errorf("Uploaded = %d, want 4 -- requests already in flight when the breaker tripped must be allowed to finish and count", stats.Uploaded)
	}

	// Check the premise rather than assuming it: if no sibling was actually
	// mid-transfer when the breaker tripped, the assertion above passes
	// without ever exercising what this test is named for.
	mu.Lock()
	siblings := siblingsInFlightAtTrip
	mu.Unlock()
	if siblings < 1 {
		t.Errorf("only %d sibling transfer(s) were in flight when the breaker tripped -- this test did not actually exercise the in-flight path, so its result proves nothing", siblings)
	}
}

// TestUploader_Run_ThrottleGetsNoPerFileRetries pins the other half of the
// redesign at the file level: a throttled file makes exactly ONE request and
// then stands aside for the breaker. It used to retry itself several times
// in place, which is precisely what kept the pipeline's request rate high
// while the rate limit was asking it to stop. Transient errors are
// unaffected and still retry per file.
func TestUploader_Run_ThrottleGetsNoPerFileRetries(t *testing.T) {
	cases := []struct {
		name string
		body string
		// requests the single file should make within one dispatch pass
		wantPerPass int
	}{
		{"throttle: handled run-wide, never retried in place", throttleBody, 1},
		{"transient: still retried in place", `{"error":{"message":"backend unavailable"}}`, maxUploadAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureBackoffs(t)
			var mu sync.Mutex
			starts := 0
			status := http.StatusTooManyRequests
			if tc.wantPerPass != 1 {
				status = http.StatusServiceUnavailable
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				starts++
				mu.Unlock()
				w.WriteHeader(status)
				fmt.Fprint(w, tc.body)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			origUploadURL := uploadURL
			uploadURL = srv.URL + "/v1/uploads"
			defer func() { uploadURL = origUploadURL }()

			db := openTestDB(t)
			dir := t.TempDir()
			p := writeFile(t, dir, "a.jpg", []byte("x"))
			mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

			u := &Uploader{
				client:      srv.Client(),
				db:          db,
				cfg:         config.Defaults(),
				dailyQuota:  quota.NewDailyQuota(db),
				concurrency: quota.NewAdaptiveConcurrency(6),
			}
			_, _ = u.Run()

			mu.Lock()
			got := starts
			mu.Unlock()

			// A throttled run repeats the single request once per breaker
			// round; a transient one is confined to a single pass. With
			// only one file in play, maxThrottleStrikes caps it at that
			// many rounds before it's benched (set aside for the next
			// Run()) rather than kept in the retry set indefinitely -- see
			// that const's doc comment.
			if tc.wantPerPass == 1 {
				wantRounds := maxThrottleStrikes
				if got != wantRounds {
					t.Errorf("server saw %d requests, want %d (exactly one per breaker round, never a retry inside a round, until maxThrottleStrikes benches it)", got, wantRounds)
				}
			} else if got != tc.wantPerPass {
				t.Errorf("server saw %d requests, want %d", got, tc.wantPerPass)
			}
		})
	}
}

// TestUploader_Run_StubbornFileBenchedAfterMaxStrikes_OthersStillProgress
// is the end-to-end version of a real report: "you try to upload a video,
// hit an error, then retry the SAME file 7 more times... skip it after 3
// attempts so a NEW file gets tried; we'll pick the skipped one back up
// once the rest of the queue is flushed." One file always throttles, one
// always succeeds -- the good one must still get uploaded, and the
// stubborn one must end up failed_retryable (picked up by the next Run()),
// not silently dropped or retried forever.
func TestUploader_Run_StubbornFileBenchedAfterMaxStrikes_OthersStillProgress(t *testing.T) {
	captureBackoffs(t)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-File-Name") == "stubborn.jpg" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, throttleBody)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	stubbornPath := writeFile(t, dir, "stubborn.jpg", []byte("stubborn"))
	mustDB(t, db.EnsurePending("hash-stubborn", 8, "image/jpeg", stubbornPath, nil))
	goodPath := writeFile(t, dir, "good.jpg", []byte("good"))
	mustDB(t, db.EnsurePending("hash-good", 4, "image/jpeg", goodPath, nil))

	var events []ProgressEvent
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		onProgress:  func(e ProgressEvent) { events = append(events, e) },
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil (stubborn file benched, not aborted)", err)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 -- the good file must succeed despite the stubborn one being stuck", stats.Uploaded)
	}
	if stats.FailedRetryable != 1 {
		t.Errorf("FailedRetryable = %d, want 1 -- the stubborn file is benched, not silently dropped", stats.FailedRetryable)
	}

	good, err := db.GetUpload("hash-good")
	if err != nil {
		t.Fatal(err)
	}
	if good.Status != "uploaded" {
		t.Errorf("good file status = %q, want uploaded", good.Status)
	}
	stubborn, err := db.GetUpload("hash-stubborn")
	if err != nil {
		t.Fatal(err)
	}
	if stubborn.Status != "failed_retryable" {
		t.Errorf("stubborn file status = %q, want failed_retryable (picked up by the next Run())", stubborn.Status)
	}

	// A file genuinely benched for burning through all its strikes on its
	// own is a REAL failure signal, not the user's own deliberate action --
	// must NOT get SkipSignal's neutral "cancelled" treatment (see
	// TestUploader_Run_SkipSignal_BenchesImmediatelyWithoutWaitingForStrikes's
	// own doc comment for the real report that distinction exists for).
	found := false
	for _, e := range events {
		if e.LastFile == stubbornPath && !e.LastDeferred {
			found = true
			if e.LastCancelled {
				t.Errorf("stubborn file's ProgressEvent.LastCancelled = true, want false -- exhausting real strikes is a genuine failure, not a user-requested skip")
			}
		}
	}
	if !found {
		t.Fatal("no ProgressEvent observed for the stubborn file at all")
	}
}

// TestUploader_Run_SkipSignal_BenchesImmediatelyWithoutWaitingForStrikes is
// the manual override maxThrottleStrikes needed, per a direct follow-up
// request: "a button to trigger the skip on the file immediately and not
// wait to those 3 tries." Requesting a skip after just ONE throttle (not
// anywhere near maxThrottleStrikes) must still bench it on the very next
// pass boundary.
// TestUploader_Run_SkipSignal_DoesNotBenchUntilStrikesExhausted is the
// corrected version of a test that used to assert the exact opposite: a
// direct correction, after the code drifted from SkipSignal's own doc
// comment back into a real regression it had already been fixed once
// before -- "no, this is a mistake, the retry now on the rung just
// zeroes the countdown, that's it." A skip request only ever ends the
// CURRENT wait sooner; it must never force a file to bench before it's
// genuinely exhausted its real strikes (maxThrottleStrikes), even if
// every single one of those waits was cut short by a fresh click. This
// re-arms skip on every throttle (onThrottle), exactly like a user
// clicking Retry Now again each time -- proving that doing so repeatedly
// still doesn't bench the file any SOONER than 3 genuine strikes would
// have on its own.
func TestUploader_Run_SkipSignal_DoesNotBenchUntilStrikesExhausted(t *testing.T) {
	captureBackoffs(t)

	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	var events []ProgressEvent
	var throttleCount int
	skip := NewSkipSignal()
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		skip:        skip,
		onThrottle: func(string, float64, int) {
			throttleCount++
			skip.Request()
		},
		onProgress: func(e ProgressEvent) { events = append(events, e) },
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil (benched after real strikes exhausted, not aborted)", err)
	}
	if throttleCount != maxThrottleStrikes {
		t.Fatalf("throttleCount = %d, want exactly %d -- a skip request must not bench the file any sooner than genuine strike exhaustion would have", throttleCount, maxThrottleStrikes)
	}
	if stats.FailedRetryable != 1 {
		t.Errorf("FailedRetryable = %d, want 1 -- benched only after %d real strikes", stats.FailedRetryable, maxThrottleStrikes)
	}

	row, gerr := db.GetUpload("hash-a")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.Status != "failed_retryable" {
		t.Errorf("status = %q, want failed_retryable", row.Status)
	}

	// This bench is a genuine strike-exhaustion, not a "you asked for
	// this" cancellation -- a skip request no longer manufactures its own
	// distinct outcome; retry-vs-bench is decided by strikes alone,
	// exactly as if every wait had run its full natural duration. Skip
	// LastDeferred events -- a file also gets one of those the moment
	// it's first set aside for the breaker; only the FINAL, non-deferred
	// event (from markRemainingRetryable) is the one this asserts on.
	found := false
	for _, e := range events {
		if e.LastFile == p && !e.LastDeferred {
			found = true
			if e.LastCancelled {
				t.Error("ProgressEvent.LastCancelled = true, want false -- a genuine strike-exhaustion is not a manual cancellation, even if every wait leading up to it was cut short by Retry Now")
			}
			if e.LastErrorKind != string(retryx.Throttle) {
				t.Errorf("LastErrorKind = %q, want %q", e.LastErrorKind, retryx.Throttle)
			}
		}
	}
	if !found {
		t.Fatal("no final (non-deferred) ProgressEvent observed for the file at all")
	}
}

// TestUploader_Run_SkipSignal_CutsAnInProgressWaitShortImmediately is the
// fix for a direct follow-up report: "the skip now in the backoff
// throtling counter is not doing anything / not working, the countown
// continues" -- SkipSignal originally only ever took effect at the next
// pass boundary (once a circuit-breaker wait completed on its own), which
// read as broken when clicked mid-countdown.
//
// Per direct clarification of what the button should actually do
// ("basically it expires the current countdown... whatever it planned to
// do in X amount of seconds - will happen now"), an early skip is NOT a
// bespoke "always bench everything" path -- it just makes the wait
// complete SOONER, and everything a normal completion already does
// (escalate the breaker, then the ordinary strikes-based retry-or-bench
// decision per row) happens exactly as it would have. This proves the
// wait itself gets interrupted (not just applied after it naturally
// finishes) AND that the breaker still escalates -- "whatever it planned
// to do... will happen now" includes the escalation, not just the
// benching.
func TestUploader_Run_SkipSignal_CutsAnInProgressWaitShortImmediately(t *testing.T) {
	origTick := backoffTickInterval
	backoffTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { backoffTickInterval = origTick })

	// throttleFirst: 1, not throttleAll -- one throttle (one mid-wait
	// skip, one rung of escalation), then the retry SUCCEEDS. A skip
	// request no longer forces a bench on its own (see SkipSignal's own
	// doc comment above), so this test's actual claim -- the wait itself
	// gets interrupted mid-countdown, and the breaker still escalates --
	// no longer needs (or should assume) a bench as its outcome.
	ts := &throttlingServer{throttleFirst: 1}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	skip := NewSkipSignal()
	breaker := NewCircuitBreakerState()

	var backoffTicks int
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		breaker:     breaker,
		skip:        skip,
		onBackoff: func(s BackoffStatus) {
			if s.Active {
				backoffTicks++
				if backoffTicks == 2 {
					skip.Request()
				}
			}
		},
	}

	done := make(chan struct{})
	var stats Stats
	var runErr error
	go func() {
		stats, runErr = u.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after Skip Now was requested mid-wait -- the wait is not interruptible by SkipSignal")
	}

	if runErr != nil {
		t.Fatalf("Run() error = %v, want nil -- a mid-wait skip moves on and the retry succeeds, it doesn't abort the run", runErr)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 -- the retry after the cut-short wait should have succeeded", stats.Uploaded)
	}

	breaker.mu.Lock()
	gotRung := breaker.rung
	breaker.mu.Unlock()
	if gotRung != 1 {
		t.Errorf("breaker.rung = %d, want 1 -- a wait cut short by Skip Now still counts as this rung having completed, same as if it had run the full duration", gotRung)
	}
}

// TestUploader_Run_SkipSignal_RequestBeforeAnyWaitIsStillHonoredImmediately
// is the actual regression: "the 'retry now' works once, and then that's
// it, the second time it just does nothing." Root cause was a
// close-and-replace broadcast channel in SkipSignal.C(): a watcher
// goroutine only captures "whichever channel is current" at the moment
// it starts, so a Request() landing in the gap between one wait's
// watcher stopping and the next one starting closed a channel nobody was
// listening to -- not lost outright (Take()'s bool still carried it), but
// silently downgraded to "apply whenever some LATER wait happens to
// complete naturally" instead of cutting the CURRENT one short, which
// looks exactly like the button doing nothing. This calls Request()
// BEFORE Run() (and therefore before any watcher exists at all) to prove
// the fix: the very first wait must still be cut short immediately, not
// left to run its real ~5s duration.
//
// throttleFirst: 1 (not throttleAll) -- the file is throttled exactly
// once, cut short by the pre-existing skip request, then SUCCEEDS on
// retry. This isolates "the first wait gets cut short" from strike
// counting entirely (a skip request no longer forces a bench on its own
// -- see SkipSignal's own doc comment -- so proving that separately here
// would just be testing the wrong thing).
func TestUploader_Run_SkipSignal_RequestBeforeAnyWaitIsStillHonoredImmediately(t *testing.T) {
	origTick := backoffTickInterval
	backoffTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { backoffTickInterval = origTick })

	ts := &throttlingServer{throttleFirst: 1}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	skip := NewSkipSignal()
	skip.Request() // the click, before Run() -- and therefore any watcher -- exists
	breaker := NewCircuitBreakerState()

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		breaker:     breaker,
		skip:        skip,
	}

	start := time.Now()
	done := make(chan struct{})
	var stats Stats
	var runErr error
	go func() {
		stats, runErr = u.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Run() did not return within 4s -- a pre-existing skip request must still cut the first wait short (rung 0 is a real 5s otherwise)")
	}
	// 3s, not 1s: the bound only has to sit clearly below the real 5s rung.
	// A tighter one measures how busy the machine is, and failed at 1.14s on
	// a Windows runner where this package takes 278s against 18s locally.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run() took %v, want well under the real 5s rung -- the pre-existing skip request should have been honored on the very first wait, not silently missed", elapsed)
	}

	if runErr != nil {
		t.Fatalf("Run() error = %v, want nil", runErr)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 -- the retry after the cut-short wait should have succeeded", stats.Uploaded)
	}
}

// TestUploader_Run_SkipSignal_InterruptsEveryConsecutiveWaitNotJustTheFirst
// is a direct regression test for the specific failure PATTERN this session
// hit twice, not just once: "the 'retry now' works once, and then that's
// it, the second time it just does nothing" (the close-and-replace channel
// bug), and later "the retry now stopped working again..." (the missing
// watcher-goroutine join). Both bugs let the FIRST skip request work fine
// and only broke on a LATER one, so a test that only ever requests once
// wouldn't have caught either. This drives ONE file through THREE
// consecutive circuit-breaker passes (throttleAll, requesting a fresh skip
// on every single throttle) and proves each of the three waits -- not just
// the first -- gets cut short immediately rather than running its real
// (escalating: 5s, 30s, 1m) duration; the file only ends up benched
// because it genuinely took 3 real strikes to get there (maxThrottleStrikes
// == 3, conveniently exactly the count this test already needs), not
// because any one of the skip requests forced it early -- see
// SkipSignal's own doc comment on why a skip is never a bench shortcut.
func TestUploader_Run_SkipSignal_InterruptsEveryConsecutiveWaitNotJustTheFirst(t *testing.T) {
	origTick := backoffTickInterval
	backoffTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { backoffTickInterval = origTick })

	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	skip := NewSkipSignal()
	breaker := NewCircuitBreakerState()

	var throttleCount int
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		breaker:     breaker,
		skip:        skip,
		onThrottle: func(string, float64, int) {
			// A fresh Request() before every single pass's wait, exactly
			// like a user clicking Retry Now again each time a new
			// countdown starts -- the scenario both real regressions
			// broke on the SECOND (and later) click, not the first.
			throttleCount++
			skip.Request()
		},
	}

	start := time.Now()
	done := make(chan struct{})
	var stats Stats
	var runErr error
	go func() {
		stats, runErr = u.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Run() did not return within 8s -- at least one of the three consecutive skip requests failed to cut its wait short (the real schedule for 3 passes is 5s+30s+1m)")
	}
	// Three consecutive waits, each cut short. The real schedule would be
	// 5s+30s+1m, so 6s still proves every one was interrupted while leaving
	// room for a slow runner.
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("Run() took %v, want well under the real schedule -- every one of the three waits should have been cut short, not just the first", elapsed)
	}

	if runErr != nil {
		t.Fatalf("Run() error = %v, want nil", runErr)
	}
	if throttleCount != maxThrottleStrikes {
		t.Fatalf("throttleCount = %d, want exactly %d -- the file should be benched at precisely its real strike count, no sooner and no later, regardless of how many skip requests preceded it", throttleCount, maxThrottleStrikes)
	}
	if stats.FailedRetryable != 1 {
		t.Errorf("FailedRetryable = %d, want 1 -- benched only after %d genuine strikes, not by any one skip request", stats.FailedRetryable, maxThrottleStrikes)
	}

	breaker.mu.Lock()
	gotRung := breaker.rung
	breaker.mu.Unlock()
	if gotRung != maxThrottleStrikes {
		t.Errorf("breaker.rung = %d, want %d -- each of the cut-short waits still counts as its rung having completed, same as a normal completion", gotRung, maxThrottleStrikes)
	}
}

// TestUploader_Run_SkipSignal_InterruptsTheFinalExhaustedRungWaitToo is the
// actual fix for a real, sharply-guessed follow-up report: "the retry now
// still doesn't work" -> "do you think that on 7/7 it gets stuck?" -- yes.
// Run() has TWO separate places that sit out a circuit-breaker wait: the
// normal per-rung wait below, and a second, separate one for when
// breaker.decayedRung() reports the schedule already exhausted (Run() pays
// for exactly one more rest at the final rung before giving up). Only the
// first ever got wired up to watch u.skip -- the second used a bare ctx
// with no watcher at all, so Retry Now was a silent no-op for the entire
// time the breaker sat at the final rung. This forces the breaker straight
// to the final rung (skipping the climb, so the test stays fast) and
// proves a skip request cuts that LAST wait short too, using the real
// (unstubbed) waitFn/backoff clock -- exactly like the mid-wait interrupt
// tests above, not the instant-complete captureBackoffs stub, since this
// bug is specifically about a REAL wait never getting interrupted.
func TestUploader_Run_SkipSignal_InterruptsTheFinalExhaustedRungWaitToo(t *testing.T) {
	origTick := backoffTickInterval
	backoffTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { backoffTickInterval = origTick })

	ts := &throttlingServer{throttleAll: true}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	skip := NewSkipSignal()
	breaker := NewCircuitBreakerState()
	// Force straight to the exhausted-schedule branch -- decayedRung()'s
	// very first call (lastChangeAt still zero) just returns rung
	// unchanged, so this alone is enough to trip `rung >= len(schedule)`
	// on the very first throttle, without actually climbing the real
	// ladder first.
	breaker.rung = len(throttleCircuitBreakerSchedule)

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		breaker:     breaker,
		skip:        skip,
		onBackoff: func(s BackoffStatus) {
			if s.Active {
				skip.Request()
			}
		},
	}

	start := time.Now()
	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = u.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return within 3s -- the final rung's real wait (15m on the real schedule) is not being cut short by a skip request")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run() took %v, want well under the final rung's real duration -- the skip request should have cut it short almost immediately", elapsed)
	}

	var abort *AbortError
	if !errors.As(runErr, &abort) || abort.Reason != AbortThrottleBackoffExhausted {
		t.Fatalf("Run() error = %v, want *AbortError{Reason: AbortThrottleBackoffExhausted} -- a skip cutting the final rest short still ends in giving up, same as if it had run the full duration", runErr)
	}
}

// TestSkipSignal_ConcurrentRequestAndTakeIsRace_Free is a package-level
// stress test on SkipSignal itself, independent of the full Uploader/Run()
// machinery -- both real races found in this mechanism this session (the
// close-and-replace channel, and the unjoined watcher goroutine) were
// synchronization bugs in exactly this type's contract ("a Request() must
// never be missed or double-applied, however it overlaps with a Take() or
// another Request()"), so a fast, direct stress test here is a cheap
// standing guard against a THIRD variant of the same class of bug, run
// under `go test -race`.
func TestSkipSignal_ConcurrentRequestAndTakeIsRace_Free(t *testing.T) {
	skip := NewSkipSignal()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			skip.Request()
		}()
		go func() {
			defer wg.Done()
			skip.Take()
		}()
		go func() {
			defer wg.Done()
			select {
			case <-skip.C():
			default:
			}
		}()
	}
	wg.Wait()
}

// TestUploader_Run_FileCancelRegistry_CancelsOneActiveFileWithoutAbortingRun
// is the fix for a real, emphatic correction: an earlier "Skip Now" only
// benched files already throttled/held for retry, but the actual ask was
// "a skip button to cancel a sync of a file and put it in a 'later-retry'
// bucket" for a file that's actively (and repeatedly) failing mid-transfer
// -- regardless of throttle state. Cancelling one file's in-flight upload
// via FileCancelRegistry must: mark ONLY that file failed_retryable
// immediately (no per-file retry, no run-wide breaker involvement), let
// every OTHER file in the same run complete normally, and not abort the
// run itself.
func TestUploader_Run_FileCancelRegistry_CancelsOneActiveFileWithoutAbortingRun(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") != "start" {
			http.Error(w, "unexpected", 400)
			return
		}
		if r.Header.Get("X-Goog-Upload-Raw-Size") == "9999" {
			// The file the test means to cancel: hang until either the
			// cancellation actually reaches this request (proving fileCtx
			// really does bound it) or the test releases it as a safety net.
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem struct {
					UploadToken string `json:"uploadToken"`
				} `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, item := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + item.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	blockPath := writeFile(t, dir, "block.jpg", bytes.Repeat([]byte("x"), 9999))
	otherPath := writeFile(t, dir, "other.jpg", []byte("small"))
	mustDB(t, db.EnsurePending("hash-block", 9999, "image/jpeg", blockPath, nil))
	mustDB(t, db.EnsurePending("hash-other", 5, "image/jpeg", otherPath, nil))

	fc := NewFileCancelRegistry()
	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
		fileCancels: fc,
	}

	done := make(chan struct{})
	var stats Stats
	var runErr error
	go func() {
		stats, runErr = u.Run()
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the blocking file's upload to start")
	}
	if !fc.Cancel(blockPath) {
		t.Fatal("Cancel returned false, want true -- the file should be registered while its upload is in flight")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run() to finish after cancelling one file")
	}

	if runErr != nil {
		t.Fatalf("Run() error = %v, want nil -- a cancelled file must not abort the whole run", runErr)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 -- the OTHER file must still complete normally", stats.Uploaded)
	}

	blockRow, gerr := db.GetUpload("hash-block")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if blockRow.Status != "failed_retryable" {
		t.Errorf("cancelled file status = %q, want failed_retryable (the \"later-retry bucket\")", blockRow.Status)
	}

	otherRow, gerr := db.GetUpload("hash-other")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if otherRow.Status != "uploaded" {
		t.Errorf("other file status = %q, want uploaded", otherRow.Status)
	}
}

// TestUploader_Run_CircuitBreaker_HeldFilesAreNotWrittenToTheLedgerYet
// proves step 1's "don't mark it failed yet": while a file is held between
// breaker rounds it must stay `pending`, not failed_retryable. Marking it
// failed early would make `gpsync info` report failures for files that are
// simply waiting, and would be undone moments later by a successful retry.
func TestUploader_Run_CircuitBreaker_HeldFilesAreNotWrittenToTheLedgerYet(t *testing.T) {
	var mu sync.Mutex
	statusDuringPause := ""

	ts := &throttlingServer{throttleFirst: 1}
	ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("held"))
	mustDB(t, db.EnsurePending("hash-held", 4, "image/jpeg", p, nil))

	// Inspect the ledger from inside the breaker's pause, which is the only
	// window where a "held" file exists.
	rec := &backoffRecorder{}
	orig := waitFn
	waitFn = func(ctx context.Context, total time.Duration, onTick func(remaining time.Duration)) bool {
		rec.record(total)
		row, err := db.GetUpload("hash-held")
		mu.Lock()
		if err == nil && row != nil {
			statusDuringPause = row.Status
		}
		mu.Unlock()
		onTick(total)
		return true
	}
	t.Cleanup(func() { waitFn = orig })

	u := &Uploader{
		client:      http.DefaultClient,
		db:          db,
		cfg:         config.Config{Concurrency: 1},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(1),
	}
	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", stats.Uploaded)
	}

	mu.Lock()
	got := statusDuringPause
	mu.Unlock()
	if got != "pending" {
		t.Errorf("status while held between breaker rounds = %q, want \"pending\" -- a file waiting on the backoff has not failed and must not be recorded as if it had", got)
	}
}

// TestUploader_Run_CircuitBreaker_LeaksNoGoroutines guards the pass-based
// restructure: every pass starts a dispatcher goroutine plus one worker per
// dispatched file, and a run that gives up does so after eight passes. If
// any of them could be left blocked -- a worker stuck sending on a results
// channel nobody drains, or a dispatcher never reaching its close -- a real
// run would slowly accumulate them, and a give-up would hang rather than
// return.
//
// Asserts on the actual stacks rather than a goroutine count: the count
// also moves with the HTTP transport's idle keep-alive connections and
// httptest's per-connection handlers, which are pooled infrastructure, not
// leaks. What matters is that no uploader goroutine survives Run.
func TestUploader_Run_CircuitBreaker_LeaksNoGoroutines(t *testing.T) {
	captureBackoffs(t)
	ts := &throttlingServer{throttleAll: true}
	srv := ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("f%02d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%02d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 4},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(4),
	}

	if _, err := u.Run(); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted", err)
	}
	srv.Client().CloseIdleConnections()

	// Retry briefly: a worker that has just returned can still appear for a
	// moment while the runtime retires it.
	var stragglers []string
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		stragglers = uploaderGoroutines()
		if len(stragglers) == 0 {
			break
		}
	}
	if len(stragglers) > 0 {
		t.Errorf("%d uploader goroutine(s) still alive after Run returned -- the dispatcher or its workers are not being cleaned up between circuit-breaker passes:\n%s",
			len(stragglers), strings.Join(stragglers, "\n---\n"))
	}
}

// uploaderGoroutines returns the stack of every live goroutine running code
// in this package, excluding the test goroutine that is asking.
func uploaderGoroutines() []string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var out []string
	for _, block := range strings.Split(string(buf[:n]), "\n\ngoroutine ") {
		if !strings.Contains(block, "github.com/gmizrahi/gpsync/internal/uploader.") {
			continue
		}
		if strings.Contains(block, "uploader.Test") || strings.Contains(block, "uploader.uploaderGoroutines") {
			continue // the test itself
		}
		out = append(out, block)
	}
	return out
}

// TestUploader_Run_ColdStartLimitsEarlyParallelism proves the slow-start
// cold start is actually observable through the dispatcher, not just inside
// AdaptiveConcurrency: a fresh run must open narrow and widen as files
// succeed, rather than putting cfg.Concurrency requests on the wire from
// file #1.
//
// This is the dispatcher's half of the contract -- its slot loop gates on
// CurrentLimit(), while the semaphore is only the hard ceiling -- so a
// limiter reporting 1 must produce one in-flight request even though the
// semaphore has room for six.
func TestUploader_Run_ColdStartLimitsEarlyParallelism(t *testing.T) {
	const configuredMax = 6

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Hold the request open long enough that genuine overlap is
		// unmissable if the dispatcher allows it.
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()

		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	// Few enough files that the ramp is still in its opening steps: the
	// limit only reaches 2 after 5 successes, so nothing here can legally
	// run 6-wide.
	const files = 6
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: configuredMax},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(configuredMax), // deliberately NOT ramped
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != files {
		t.Fatalf("Uploaded = %d, want %d -- a narrow opening must not cost any files", stats.Uploaded, files)
	}

	mu.Lock()
	got := maxInFlight
	mu.Unlock()
	if got > 2 {
		t.Errorf("peak in-flight requests = %d over the first %d files, want <= 2 -- a fresh run is opening at full configured concurrency (%d) instead of slow-starting, which is exactly what trips Google's concurrent-write ceiling",
			got, files, configuredMax)
	}
	if got < 1 {
		t.Errorf("peak in-flight = %d -- the server never saw a request", got)
	}
}

// TestUploader_SharedConcurrencyLimiterCarriesAcrossFolders proves why the
// adaptive limiter is built once per INVOCATION and handed to every
// folder's Uploader, rather than each Uploader constructing its own.
//
// `gpsync sync`/`gpsync upload` process a library folder by folder, and the
// limiter is what the run has learned about the safe request rate -- in
// both directions: how far it had to back off after a throttle, and how far
// it has climbed since. Rebuilding it per folder discards that at every
// folder boundary and restarts the cold-start ramp from 1. For the shape
// this tool exists for (year/month folders of ~20-50 files) that means most
// of every folder runs at low concurrency and the configured maximum is
// rarely reached -- excessive caution, and it also forgets a throttle it
// just backed off from.
//
// Two folders of 6 files each, configuredMax 8: sharing one limiter must
// leave it strictly higher than rebuilding it per folder.
func TestUploader_SharedConcurrencyLimiterCarriesAcrossFolders(t *testing.T) {
	const (
		configuredMax  = 8
		filesPerFolder = 6
	)

	fake := newFakeGooglePhotos()
	srv := fake.server()
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	root := t.TempDir()

	// Two folders' worth of pending work, scoped the way the per-folder
	// loop in cmd/gpsync scopes them.
	folders := make([]string, 2)
	for f := 0; f < 2; f++ {
		folders[f] = filepath.Join(root, fmt.Sprintf("folder%d", f))
		if err := os.MkdirAll(folders[f], 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < filesPerFolder; i++ {
			name := fmt.Sprintf("f%d.jpg", i)
			content := fmt.Sprintf("folder%d-file%d", f, i)
			p := writeFile(t, folders[f], name, []byte(content))
			mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d-%d", f, i), int64(len(content)), "image/jpeg", p, nil))
		}
	}

	runFolder := func(folder string, conc *quota.AdaptiveConcurrency) {
		t.Helper()
		u := &Uploader{
			client:       srv.Client(),
			db:           db,
			cfg:          config.Config{Concurrency: configuredMax},
			dailyQuota:   quota.NewDailyQuota(db),
			concurrency:  conc,
			scopeFolders: []string{folder},
		}
		stats, err := u.Run()
		if err != nil {
			t.Fatalf("Run(%s) error: %v", folder, err)
		}
		if stats.Uploaded != filesPerFolder {
			t.Fatalf("Run(%s) uploaded %d, want %d", folder, stats.Uploaded, filesPerFolder)
		}
	}

	// Folder 1, cold start. Both scenarios below share this starting point.
	shared := quota.NewAdaptiveConcurrency(configuredMax)
	runFolder(folders[0], shared)
	afterFirst := shared.CurrentLimit()
	if afterFirst <= 1 {
		t.Fatalf("limit after the first folder = %d, want > 1 -- the ramp is not climbing at all", afterFirst)
	}

	// Folder 2 carrying the same limiter forward...
	runFolder(folders[1], shared)
	sharedFinal := shared.CurrentLimit()

	// ...versus what a per-folder limiter would have reached: folder 2 all
	// over again from a cold start.
	perFolder := quota.NewAdaptiveConcurrency(configuredMax)
	for i := 0; i < filesPerFolder; i++ {
		perFolder.OnSuccess()
	}
	perFolderFinal := perFolder.CurrentLimit()

	if sharedFinal <= perFolderFinal {
		t.Errorf("after two folders the shared limiter is at %d, no better than a per-folder limiter's %d -- the ramp is being restarted at each folder boundary instead of carrying forward",
			sharedFinal, perFolderFinal)
	}
	if sharedFinal != afterFirst*2 {
		t.Errorf("shared limiter went %d -> %d across the two folders, want a continued doubling to %d", afterFirst, sharedFinal, afterFirst*2)
	}
}

// TestUploader_SharedConcurrencyLimiterKeepsAThrottleBackoff is the other
// direction of the same property, and the one that actually protects the
// API: a folder that got throttled has halved the limit, and the NEXT
// folder in the same invocation must inherit that reduced level rather than
// cheerfully reopening at the rate that just got rate-limited.
func TestUploader_SharedConcurrencyLimiterKeepsAThrottleBackoff(t *testing.T) {
	shared := rampedConcurrency(t, 8)
	if got := shared.CurrentLimit(); got != 8 {
		t.Fatalf("setup: limit = %d, want 8", got)
	}

	// Folder 1 hits a throttle: the circuit breaker halves the ceiling.
	shared.OnThrottled()
	backedOff := shared.CurrentLimit()
	if backedOff != 4 {
		t.Fatalf("limit after a throttle = %d, want 4", backedOff)
	}

	// Folder 2 starts here, not back at 8 and not back at 1.
	if got := shared.CurrentLimit(); got != backedOff {
		t.Errorf("the next folder would start at %d, want %d -- a shared limiter must carry a throttle backoff across the folder boundary", got, backedOff)
	}
}

// TestRealBackoffWait_TicksDownInSteps unit-tests the pause clock itself.
// It sleeps in short steps rather than one long call so a paused run can
// report a countdown: with dispatch stopped, no upload progress events fire
// at all, so without these ticks a 15-minute rung is indistinguishable
// on screen from a hang.
func TestRealBackoffWait_TicksDownInSteps(t *testing.T) {
	origInterval := backoffTickInterval
	backoffTickInterval = 10 * time.Millisecond
	defer func() { backoffTickInterval = origInterval }()

	const total = 60 * time.Millisecond
	var ticks []time.Duration
	start := time.Now()
	realBackoffWait(context.Background(), total, func(remaining time.Duration) {
		ticks = append(ticks, remaining)
	})
	elapsed := time.Since(start)

	if len(ticks) < 3 {
		t.Fatalf("got %d ticks (%v) over a %v pause with a %v interval, want several -- it is still sleeping in one solid call", len(ticks), ticks, total, backoffTickInterval)
	}
	if ticks[0] != total {
		t.Errorf("first tick reported %v remaining, want the full %v -- the display must be correct the instant the pause starts", ticks[0], total)
	}
	for i := 1; i < len(ticks); i++ {
		if ticks[i] >= ticks[i-1] {
			t.Errorf("tick %d reported %v remaining, not less than the previous %v -- the countdown must actually count down: %v", i, ticks[i], ticks[i-1], ticks)
			break
		}
	}
	if last := ticks[len(ticks)-1]; last > backoffTickInterval {
		t.Errorf("last tick reported %v remaining, want <= one interval (%v)", last, backoffTickInterval)
	}
	if elapsed < total {
		t.Errorf("returned after %v, want at least the full %v -- ticking must not shorten the pause", elapsed, total)
	}
}

// TestRealBackoffWait_ShortPauseStillTicksOnce: a rung shorter than one
// tick interval must still report itself once rather than passing silently.
func TestRealBackoffWait_ShortPauseStillTicksOnce(t *testing.T) {
	origInterval := backoffTickInterval
	backoffTickInterval = time.Second
	defer func() { backoffTickInterval = origInterval }()

	var ticks []time.Duration
	realBackoffWait(context.Background(), 5*time.Millisecond, func(remaining time.Duration) {
		ticks = append(ticks, remaining)
	})
	if len(ticks) != 1 || ticks[0] != 5*time.Millisecond {
		t.Errorf("ticks = %v, want exactly one tick of 5ms", ticks)
	}
}

// TestUploader_Run_OnBackoff_ReportsALiveCountdownThenClears is the
// end-to-end version: a real run, a real throttle, the real pause clock
// (just with a short schedule and tick interval), proving the callback a
// live view depends on actually fires repeatedly during the pause and is
// cleared afterwards.
//
// Before this, a circuit-breaker pause emitted exactly one static line and
// then nothing for up to 15 minutes -- no countdown, no rung, no indication
// of the concurrency it would resume at.
func TestUploader_Run_OnBackoff_ReportsALiveCountdownThenClears(t *testing.T) {
	origSchedule, origInterval := throttleCircuitBreakerSchedule, backoffTickInterval
	throttleCircuitBreakerSchedule = []time.Duration{60 * time.Millisecond}
	backoffTickInterval = 10 * time.Millisecond
	defer func() { throttleCircuitBreakerSchedule, backoffTickInterval = origSchedule, origInterval }()

	// Throttle exactly one request (see the ResetsAndResumes test for why
	// exactly one), then serve normally, so the run pauses once and resumes.
	ts := &throttlingServer{throttleFirst: 1}
	srv := ts.serve(t)

	const files = 4
	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	var mu sync.Mutex
	var statuses []BackoffStatus
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 6},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 6),
		onBackoff: func(s BackoffStatus) {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if stats.Uploaded != files {
		t.Fatalf("Uploaded = %d, want %d", stats.Uploaded, files)
	}

	mu.Lock()
	got := append([]BackoffStatus(nil), statuses...)
	mu.Unlock()

	if len(got) < 4 {
		t.Fatalf("onBackoff fired %d times, want several ticks plus a clear -- a pause must not be a single static event: %+v", len(got), got)
	}

	// Everything but the last must be an active tick, counting down.
	active := got[:len(got)-1]
	for i, s := range active {
		if !s.Active {
			t.Fatalf("status %d is inactive mid-pause: %+v", i, s)
		}
		if i > 0 && s.RemainingWait >= active[i-1].RemainingWait {
			t.Errorf("tick %d reports %v remaining, not less than the previous %v -- the countdown is not advancing", i, s.RemainingWait, active[i-1].RemainingWait)
			break
		}
	}

	first := active[0]
	if first.RemainingWait != throttleCircuitBreakerSchedule[0] {
		t.Errorf("first tick RemainingWait = %v, want the full rung %v", first.RemainingWait, throttleCircuitBreakerSchedule[0])
	}
	if first.RungWait != throttleCircuitBreakerSchedule[0] {
		t.Errorf("RungWait = %v, want %v", first.RungWait, throttleCircuitBreakerSchedule[0])
	}
	if first.Rung != 1 || first.TotalRungs != 1 {
		t.Errorf("rung = %d/%d, want 1/1", first.Rung, first.TotalRungs)
	}
	if first.Reason == "" || !strings.Contains(first.Reason, "concurrent write request") {
		t.Errorf("Reason = %q, want the throttle message that tripped the breaker", first.Reason)
	}
	if first.MaxConcurrency != 6 {
		t.Errorf("MaxConcurrency = %d, want 6 (the configured ceiling)", first.MaxConcurrency)
	}
	// The throttle halved 6 -> 3; the display promises where dispatch resumes.
	if first.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3 (halved by the throttle)", first.Concurrency)
	}

	// ...and the pause must clear itself, or a live view is left showing
	// "paused" while the pipeline is actually running again.
	last := got[len(got)-1]
	if last.Active {
		t.Errorf("final status is still Active: %+v -- the display would stay stuck on 'throttled' after the run resumed", last)
	}
}

// TestUploader_Run_OnBackoff_ClearsWhenTheRunGivesUp: the same clear must
// arrive when the schedule is exhausted and the run aborts. The abort's own
// messaging takes over from there, but a live view must first stop showing
// a countdown that is no longer counting.
func TestUploader_Run_OnBackoff_ClearsWhenTheRunGivesUp(t *testing.T) {
	origSchedule := throttleCircuitBreakerSchedule
	throttleCircuitBreakerSchedule = []time.Duration{time.Second, 2 * time.Second}
	defer func() { throttleCircuitBreakerSchedule = origSchedule }()
	captureBackoffs(t) // drive the clock; one tick per pause

	ts := &throttlingServer{throttleAll: true}
	srv := ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	var mu sync.Mutex
	var statuses []BackoffStatus
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onBackoff: func(s BackoffStatus) {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		},
	}
	if _, err := u.Run(); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Run error = %v, want ErrRunAborted", err)
	}

	mu.Lock()
	got := append([]BackoffStatus(nil), statuses...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatal("onBackoff never fired")
	}
	if got[len(got)-1].Active {
		t.Errorf("final status is still Active: %+v -- a run that gave up must not leave a live countdown on screen", got[len(got)-1])
	}
	// One active tick + one clear per rung.
	var clears int
	for _, s := range got {
		if !s.Active {
			clears++
		}
	}
	if clears != len(throttleCircuitBreakerSchedule) {
		t.Errorf("got %d clears across %d pauses, want one each -- every pause must clear itself", clears, len(throttleCircuitBreakerSchedule))
	}
}

// TestUploader_Run_OnBackoff_NilCallbackIsSafe: onBackoff is optional, like
// every other callback, and a pause must still work without it.
func TestUploader_Run_OnBackoff_NilCallbackIsSafe(t *testing.T) {
	rec := captureBackoffs(t)
	ts := &throttlingServer{throttleFirst: 1}
	srv := ts.serve(t)

	db := openTestDB(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jpg", []byte("x"))
	mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
		onBackoff:   nil,
	}
	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if stats.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1", stats.Uploaded)
	}
	// The pause still happened, callback or not.
	if d := rec.durations(); len(d) != 1 || d[0] != throttleCircuitBreakerSchedule[0] {
		t.Errorf("backoffs = %v, want one pause at the first rung", d)
	}
}

// TestUploader_Run_RecordsHowItEndedInTheLedger proves every exit path
// leaves a usable record of the outcome in run_progress, read back from the
// DB rather than inferred from the returned error.
//
// Both abort paths used to call plain RunFinish, so a run that gave up on a
// throttle or on the daily quota was indistinguishable in the ledger from
// one that finished its work -- `gpsync info` had nothing to report and the
// user's only clue was files silently sitting pending.
func TestUploader_Run_RecordsHowItEndedInTheLedger(t *testing.T) {
	t.Run("throttle backoff exhausted", func(t *testing.T) {
		captureBackoffs(t)
		// Shortened to 2 rungs so the RUN-WIDE breaker reliably exhausts
		// (the scenario this subtest exists to check) well before any
		// single file could rack up maxThrottleStrikes (3) on its own --
		// at concurrency 2 spread over 3 files, per-file strikes
		// accumulate slower than one per pass, so the real (longer)
		// default schedule leaves enough passes for individual files to
		// bench out first instead, exiting cleanly (nil error) rather
		// than via the schedule-exhausted abort this subtest asserts on.
		origSchedule := throttleCircuitBreakerSchedule
		throttleCircuitBreakerSchedule = []time.Duration{time.Second, 2 * time.Second}
		t.Cleanup(func() { throttleCircuitBreakerSchedule = origSchedule })
		ts := &throttlingServer{throttleAll: true}
		srv := ts.serve(t)

		db := openTestDB(t)
		dir := t.TempDir()
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("f%d.jpg", i)
			p := writeFile(t, dir, name, []byte(name))
			mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
		}

		u := &Uploader{
			client:      srv.Client(),
			db:          db,
			cfg:         config.Config{Concurrency: 2},
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(2),
		}
		if _, err := u.Run(); !errors.Is(err, ErrRunAborted) {
			t.Fatalf("Run error = %v, want ErrRunAborted", err)
		}

		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run.EndReason != statedb.EndReasonAbortedThrottle {
			t.Errorf("EndReason = %q, want %q", run.EndReason, statedb.EndReasonAbortedThrottle)
		}
		if run.Status == "running" {
			t.Error("run left at status='running' -- an aborted run must close itself out")
		}
		for _, want := range []string{"backoff", "left pending"} {
			if !strings.Contains(run.EndDetail, want) {
				t.Errorf("EndDetail = %q, want it to mention %q", run.EndDetail, want)
			}
		}
	})

	t.Run("daily quota exhausted", func(t *testing.T) {
		db := openTestDB(t)
		// Proactively exhausted: Run stops before sending anything.
		mustDB(t, db.QuotaIncrement(quota.PacificTodayStr(), 9800))
		dir := t.TempDir()
		p := writeFile(t, dir, "a.jpg", []byte("x"))
		mustDB(t, db.EnsurePending("hash-a", 1, "image/jpeg", p, nil))

		u := &Uploader{
			client:      http.DefaultClient,
			db:          db,
			cfg:         config.Defaults(),
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(6),
		}
		if _, err := u.Run(); !errors.Is(err, ErrRunAborted) {
			t.Fatalf("Run error = %v, want ErrRunAborted", err)
		}

		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run.EndReason != statedb.EndReasonAbortedDailyQuota {
			t.Errorf("EndReason = %q, want %q", run.EndReason, statedb.EndReasonAbortedDailyQuota)
		}
		if !strings.Contains(run.EndDetail, "quota") {
			t.Errorf("EndDetail = %q, want it to explain the quota exhaustion", run.EndDetail)
		}
	})

	t.Run("completed normally", func(t *testing.T) {
		fake := newFakeGooglePhotos()
		srv := fake.server()
		defer srv.Close()

		origUploadURL, origBatchURL := uploadURL, batchCreateURL
		uploadURL = srv.URL + "/v1/uploads"
		batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
		defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

		db := openTestDB(t)
		dir := t.TempDir()
		p := writeFile(t, dir, "a.jpg", []byte("content-a"))
		mustDB(t, db.EnsurePending("hash-a", 9, "image/jpeg", p, nil))

		u := &Uploader{
			client:      srv.Client(),
			db:          db,
			cfg:         config.Defaults(),
			dailyQuota:  quota.NewDailyQuota(db),
			concurrency: quota.NewAdaptiveConcurrency(6),
		}
		if _, err := u.Run(); err != nil {
			t.Fatal(err)
		}
		run, err := db.RunGet()
		if err != nil {
			t.Fatal(err)
		}
		if run.EndReason != statedb.EndReasonCompleted {
			t.Errorf("EndReason = %q, want %q", run.EndReason, statedb.EndReasonCompleted)
		}
	})
}

// realConcurrentWriteThrottleBody is the exact response Google returned in
// the production incident this test reproduces. 'concurrent write request'
// is the quota that mediaItems.batchCreate burns -- the raw byte-upload
// endpoint is a different service with a different budget -- so in real use
// a throttle lands overwhelmingly on batchCreate, not on the uploads that
// precede it.
const realConcurrentWriteThrottleBody = `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'."}}`

// TestUploader_Run_ThrottleOnBatchCreate_TripsTheCircuitBreaker reproduces
// a production failure the rest of this file missed entirely.
//
// Symptom, from a real 20-folder `gpsync sync`: files failing in bursts of
// ~17-20 at a single timestamp with "Quota exceeded for quota 'concurrent
// write request'", 35 landing as failed_retryable, the whole batch finishing
// in 37 seconds, and ZERO throttle output of any kind -- no pause, no
// "⏸ THROTTLED" section, not even the one-shot onThrottle scrollback line.
// Exactly the pre-circuit-breaker behavior.
//
// Cause: every throttle test in this file throttled /v1/uploads, so the
// breaker was only ever exercised on the byte-upload path. batchCreate's
// error handling classified the response and then marked every file in the
// batch failed_retryable, without consulting reclassifyDailyQuota, without
// tripping the breaker, and without firing onThrottle -- which is why the
// run was silent. The burst-of-20 shape is the tell: those are batch-sized
// groups (batchFlushCount), not per-file failures.
//
// The bytes upload fine here and only batchCreate throttles, mirroring
// reality.
func TestUploader_Run_ThrottleOnBatchCreate_TripsTheCircuitBreaker(t *testing.T) {
	rec := captureBackoffs(t)

	var mu sync.Mutex
	batchCreateCalls := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		batchCreateCalls++
		n := batchCreateCalls
		mu.Unlock()
		if n == 1 {
			// The write quota is exhausted; the bytes were fine.
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, realConcurrentWriteThrottleBody)
			return
		}
		batchCreateOKHandler(w, r)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	// More than batchFlushCount, so a full-sized batch really forms -- the
	// production burst was ~20 files failing together.
	const files = batchFlushCount + 5
	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%03d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%03d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	var throttleAnnouncements, backoffTicks int
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 6},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 6), // folder 20 of a batch: already ramped up
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			if backoffSeconds > 0 {
				throttleAnnouncements++
			}
		},
		onBackoff: func(s BackoffStatus) {
			if s.Active {
				backoffTicks++
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil (the throttle clears on the retry)", err)
	}

	// The run must have actually PAUSED.
	if got := rec.durations(); len(got) == 0 {
		t.Errorf("the run never paused -- a batchCreate throttle is not reaching the circuit breaker at all (this is the production bug: 20 folders finished in 37s with no backoff)")
	} else if got[0] != throttleCircuitBreakerSchedule[0] {
		t.Errorf("first pause = %v, want the first rung %v", got[0], throttleCircuitBreakerSchedule[0])
	}
	if throttleAnnouncements == 0 {
		t.Error("onThrottle never fired -- the production run showed no throttle output whatsoever, which is exactly this")
	}
	if backoffTicks == 0 {
		t.Error("onBackoff never fired -- the live '⏸ THROTTLED' section would never appear")
	}

	// And the throttled files must be HELD, not written off. In production
	// all 20-odd were marked failed_retryable in one go.
	if stats.FailedRetryable != 0 {
		t.Errorf("FailedRetryable = %d, want 0 -- a batch throttled by the write quota must be held for retry, not failed", stats.FailedRetryable)
	}
	if stats.Uploaded != files {
		t.Errorf("Uploaded = %d, want %d -- every file should land once the throttle clears", stats.Uploaded, files)
	}
	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["failed_retryable"] != 0 {
		t.Errorf("ledger has %d failed_retryable rows, want 0: %+v", counts["failed_retryable"], counts)
	}
	if counts["uploaded"] != files {
		t.Errorf("ledger counts = %+v, want %d uploaded", counts, files)
	}
}

// TestUploader_Run_ThrottleOnBatchCreate_RecordsThrottleEvent covers the
// telemetry this backs: a direct request to "make sure we have a proper
// log to track this... when did we start the backoff... so in the end, we
// will be able to determine if 60m our 1h or perhaps 1.5h is the window."
// Same shape as TestUploader_Run_ThrottleOnBatchCreate_TripsTheCircuitBreaker
// just above, but asserting on db.ThrottleEvents() instead of the live
// onThrottle/onBackoff callbacks.
func TestUploader_Run_ThrottleOnBatchCreate_RecordsThrottleEvent(t *testing.T) {
	rec := captureBackoffs(t)
	_ = rec

	var mu sync.Mutex
	batchCreateCalls := 0

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		batchCreateCalls++
		n := batchCreateCalls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, realConcurrentWriteThrottleBody)
			return
		}
		batchCreateOKHandler(w, r)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	const files = batchFlushCount + 5
	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%03d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%03d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 6},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 6),
	}

	if _, err := u.Run(); err != nil {
		t.Fatalf("Run error = %v, want nil (the throttle clears on the retry)", err)
	}

	events, err := db.ThrottleEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("ThrottleEvents() is empty -- the real concurrent-write throttle path never called RecordThrottleEvent")
	}
	ev := events[0]
	if ev.Rung != 0 {
		t.Errorf("Rung = %d, want 0 (first backoff step)", ev.Rung)
	}
	if ev.WaitSeconds != throttleCircuitBreakerSchedule[0].Seconds() {
		t.Errorf("WaitSeconds = %v, want %v", ev.WaitSeconds, throttleCircuitBreakerSchedule[0].Seconds())
	}
	if !strings.Contains(ev.Message, "concurrent write request") {
		t.Errorf("Message = %q, want it to contain the throttle reason", ev.Message)
	}
}

// erroringOnceTransport fails the first request whose URL contains path
// with a network-level error (no HTTP response at all -- the same shape as
// a real client-side timeout), then passes every other request through.
type erroringOnceTransport struct {
	next      http.RoundTripper
	path      string
	err       error
	mu        sync.Mutex
	triggered bool
}

func (t *erroringOnceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	fire := !t.triggered && strings.Contains(req.URL.Path, t.path)
	if fire {
		t.triggered = true
	}
	t.mu.Unlock()
	if fire {
		return nil, t.err
	}
	return t.next.RoundTrip(req)
}

// TestUploader_Run_NetworkErrorOnBatchCreate_TripsTheCircuitBreaker covers
// the other way a batchCreate call can fail: not an HTTP throttle response
// (see TestUploader_Run_ThrottleOnBatchCreate_TripsTheCircuitBreaker just
// above), but u.client.Post itself returning a network-level error -- most
// plausibly a client-side timeout (baseHTTPClient's ResponseHeaderTimeout
// is 30s) when a big batch takes Google longer to process. This used to
// mark every file in the batch failed_retryable immediately with zero
// circuit-breaker engagement: a real production report ("it seems you are
// trying to batch so many files at once, and feels like google doesn't
// like it. it fails quite ugly", no improvement after 12+ hours plus a
// full daily-quota reset) pointed straight at this -- a client bug that
// re-triggers on every attempt regardless of how long the account rests,
// since it never depended on Google's state in the first place.
func TestUploader_Run_NetworkErrorOnBatchCreate_TripsTheCircuitBreaker(t *testing.T) {
	rec := captureBackoffs(t)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", batchCreateOKHandler)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	const files = batchFlushCount + 5
	db := openTestDB(t)
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%03d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%03d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	transport := &erroringOnceTransport{
		next: srv.Client().Transport,
		path: "/v1/mediaItems:batchCreate",
		err:  errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"),
	}

	var throttleAnnouncements, backoffTicks int
	u := &Uploader{
		client:      &http.Client{Transport: transport},
		db:          db,
		cfg:         config.Config{Concurrency: 6},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 6),
		onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
			if backoffSeconds > 0 {
				throttleAnnouncements++
			}
		},
		onBackoff: func(s BackoffStatus) {
			if s.Active {
				backoffTicks++
			}
		},
	}

	stats, err := u.Run()
	if err != nil {
		t.Fatalf("Run error = %v, want nil (the network error clears on the retry)", err)
	}

	if !transport.triggered {
		t.Fatal("the test never actually forced a network-level error -- the case is not exercising what it claims")
	}
	if got := rec.durations(); len(got) == 0 {
		t.Error("the run never paused -- a network-level batchCreate error is not reaching the circuit breaker at all")
	} else if got[0] != throttleCircuitBreakerSchedule[0] {
		t.Errorf("first pause = %v, want the first rung %v", got[0], throttleCircuitBreakerSchedule[0])
	}
	if throttleAnnouncements == 0 {
		t.Error("onThrottle never fired for a network-level batchCreate error")
	}
	if backoffTicks == 0 {
		t.Error("onBackoff never fired for a network-level batchCreate error")
	}

	// The files must be HELD, not written off -- the bug wrote every file
	// in the batch off as failed_retryable instantly, with no backoff.
	if stats.FailedRetryable != 0 {
		t.Errorf("FailedRetryable = %d, want 0 -- a network error on batchCreate must be held for retry, not failed", stats.FailedRetryable)
	}
	if stats.Uploaded != files {
		t.Errorf("Uploaded = %d, want %d -- every file should land once the retry succeeds", stats.Uploaded, files)
	}
	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["failed_retryable"] != 0 {
		t.Errorf("ledger has %d failed_retryable rows, want 0: %+v", counts["failed_retryable"], counts)
	}
	if counts["uploaded"] != files {
		t.Errorf("ledger counts = %+v, want %d uploaded", counts, files)
	}
}

// TestUploader_Run_EveryWriteEndpointTripsTheCircuitBreaker is the
// structural guard against the bug class that let the batchCreate throttle
// through: coverage that is specific to ONE endpoint.
//
// Google's 'concurrent write request' quota is spent by every write call in
// the pipeline -- the byte upload and mediaItems.batchCreate -- and a
// throttle from either of them must back the whole pipeline off. (It used
// to cover albums.create/batchAddMediaItems too, until album support was
// removed outright -- see migrateDropAlbumTables.) Previously only the
// first had any
// throttle handling at all; the others classified the response and then
// either wrote the batch off as failed or discarded the error entirely, so
// a run could sit inside a sustained rate limit indefinitely without ever
// pausing or printing a single throttle line.
//
// Each case throttles exactly one endpoint, once, then serves normally.
func TestUploader_Run_EveryWriteEndpointTripsTheCircuitBreaker(t *testing.T) {
	const files = 3

	cases := []struct {
		name string
		// endpoint is matched against the request path to decide what to
		// throttle exactly once.
		throttlePath string
		// filesStillUpload is false only where the throttle prevents the
		// media items being created at all.
		filesStillUpload bool
	}{
		{name: "byte upload", throttlePath: "/v1/uploads", filesStillUpload: true},
		{name: "mediaItems.batchCreate", throttlePath: "/v1/mediaItems:batchCreate", filesStillUpload: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureBackoffs(t)

			var mu sync.Mutex
			throttledOnce := false
			// throttleIfTarget reports whether this request should be the
			// one throttled response for this case.
			throttleIfTarget := func(path string) bool {
				mu.Lock()
				defer mu.Unlock()
				if throttledOnce || path != tc.throttlePath {
					return false
				}
				throttledOnce = true
				return true
			}
			sendThrottle := func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, realConcurrentWriteThrottleBody)
			}

			mux := http.NewServeMux()
			var srv *httptest.Server

			mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
				if throttleIfTarget("/v1/uploads") {
					sendThrottle(w)
					return
				}
				w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
				w.Write(body)
			})
			mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
				if throttleIfTarget("/v1/mediaItems:batchCreate") {
					sendThrottle(w)
					return
				}
				batchCreateOKHandler(w, r)
			})

			srv = httptest.NewServer(mux)
			defer srv.Close()

			origUploadURL, origBatchURL := uploadURL, batchCreateURL
			uploadURL = srv.URL + "/v1/uploads"
			batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
			defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

			db := openTestDB(t)
			dir := filepath.Join(t.TempDir(), "Vacation2024")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("f%d.jpg", i)
				content := fmt.Sprintf("content-%d", i)
				p := writeFile(t, dir, name, []byte(content))
				mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(content)), "image/jpeg", p, nil))
			}

			var throttleAnnouncements int
			u := &Uploader{
				client:      srv.Client(),
				db:          db,
				cfg:         config.Config{Concurrency: 4},
				dailyQuota:  quota.NewDailyQuota(db),
				concurrency: rampedConcurrency(t, 4),
				onThrottle: func(message string, backoffSeconds float64, newConcurrency int) {
					if backoffSeconds > 0 {
						throttleAnnouncements++
					}
				},
			}

			stats, err := u.Run()
			if err != nil {
				t.Fatalf("Run error = %v, want nil", err)
			}

			// The one assertion that matters: the pipeline actually backed off.
			if got := rec.durations(); len(got) == 0 {
				t.Errorf("a throttle from %s never paused the run -- this endpoint is not wired to the circuit breaker", tc.throttlePath)
			}
			if throttleAnnouncements == 0 {
				t.Errorf("a throttle from %s produced no throttle output at all", tc.throttlePath)
			}
			if !throttledOnce {
				t.Errorf("the test server never got a chance to throttle %s -- the case is not exercising what it claims", tc.throttlePath)
			}
			if tc.filesStillUpload && stats.Uploaded != files {
				t.Errorf("Uploaded = %d, want %d -- backing off must not cost files", stats.Uploaded, files)
			}
			if stats.FailedRetryable != 0 || stats.FailedPermanent != 0 {
				t.Errorf("unexpected failures %+v -- a throttled write is held and retried, never written off", stats)
			}
		})
	}
}

// TestUploader_Run_SustainedBatchCreateThrottle_ExhaustsScheduleAndAborts:
// the give-up path must work through batchCreate too, not just the byte
// upload -- otherwise a genuinely stuck write quota would loop forever or,
// as in production, silently write every batch off.
// TestUploader_Run_SustainedBatchCreateThrottle_BenchesAfterMaxStrikes
// covers batchCreate throttling specifically because it behaves
// differently from byte-upload throttling under maxThrottleStrikes: every
// file's byte upload succeeds immediately here (only batchCreate ever
// throttles), so they all accumulate into ONE pendingBatch and get
// deferred TOGETHER as a single unit each round -- there's no equivalent
// of undispatchedRows staggering individual files' strike counts apart
// (that only happens for byte-upload-level throttling, see
// TestUploader_Run_CircuitBreaker_ElapsedTimeWithoutProgressDoesNotDecay,
// which still covers the full-schedule-exhaustion path for that case).
// So for a batch small enough to flush as one unit, EVERY file in it hits
// maxThrottleStrikes at the same round and gets benched together -- the
// run completes normally (not aborted) once the queue empties, well
// short of the full 7-rung schedule.
func TestUploader_Run_SustainedBatchCreateThrottle_BenchesAfterMaxStrikes(t *testing.T) {
	rec := captureBackoffs(t)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, realConcurrentWriteThrottleBody)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	const files = 4
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%d.jpg", i)
		p := writeFile(t, dir, name, []byte(name))
		mustDB(t, db.EnsurePending(fmt.Sprintf("hash-%d", i), int64(len(name)), "image/jpeg", p, nil))
	}

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Config{Concurrency: 4},
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: rampedConcurrency(t, 4),
	}

	stats, err := u.Run()
	// Every file lands in the SAME batch (well under batchFlushCount) and
	// gets deferred together each round, so all 4 hit maxThrottleStrikes
	// at the same round and get benched together -- the queue empties and
	// Run() returns normally, well short of the full schedule.
	if err != nil {
		t.Fatalf("Run error = %v, want nil (benched, not aborted)", err)
	}
	if got := rec.durations(); len(got) != maxThrottleStrikes {
		t.Errorf("paused %d times (%v), want %d (one per strike before benching)", len(got), got, maxThrottleStrikes)
	}
	if stats.FailedRetryable != files {
		t.Errorf("FailedRetryable = %d, want %d -- everyone gets benched together once they've all hit the strike cap", stats.FailedRetryable, files)
	}
	run, err := db.RunGet()
	if err != nil {
		t.Fatal(err)
	}
	if run.EndReason != statedb.EndReasonCompleted {
		t.Errorf("EndReason = %q, want %q -- the run did finish, it just benched everything", run.EndReason, statedb.EndReasonCompleted)
	}
}

// TestUploader_Run_SendsOriginalFileNameToGoogle reproduces a real
// data-traceability failure: photos uploaded by gpsync showed up in Google
// Photos with no usable filename, leaving the user unable to tell which
// local file a given uploaded photo came from.
//
// Cause: mediaItems.batchCreate was sent with only an uploadToken. Google's
// API reference is explicit that SimpleMediaItem.fileName is "File name
// with extension of the media item. This is shown to the user in Google
// Photos." -- it is the field that determines the name, and gpsync never set
// it. (X-Goog-Upload-File-Name is NOT part of the Photos API's documented
// resumable-start header set, and per the same reference is ignored
// whenever fileName is set, so fileName is the fix that matters.)
//
// The name must be the ORIGINAL basename, which is not always the path the
// bytes came from: in space-saver mode the bytes are a temp file with a
// generated name, and sending that would be worse than sending nothing.
func TestUploader_Run_SendsOriginalFileNameToGoogle(t *testing.T) {
	type simpleItem struct {
		UploadToken string `json:"uploadToken"`
		FileName    string `json:"fileName"`
	}
	var mu sync.Mutex
	var gotItems []simpleItem
	var gotUploadFileNameHeader string

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if h := r.Header.Get("X-Goog-Upload-File-Name"); h != "" {
			gotUploadFileNameHeader = h
		}
		mu.Unlock()
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem simpleItem `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, it := range req.NewMediaItems {
			gotItems = append(gotItems, it.SimpleMediaItem)
		}
		mu.Unlock()

		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		for _, it := range req.NewMediaItems {
			var ri resultItem
			ri.MediaItem.ID = "media-" + it.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	const name = "20230928_182735.jpg" // the shape of a real filename from the report
	p := writeFile(t, dir, name, []byte("photo-bytes"))
	mustDB(t, db.EnsurePending("hash-named", 11, "image/jpeg", p, nil))

	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         config.Defaults(),
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
	}
	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", stats.Uploaded)
	}

	mu.Lock()
	items := append([]simpleItem(nil), gotItems...)
	header := gotUploadFileNameHeader
	mu.Unlock()

	if len(items) != 1 {
		t.Fatalf("batchCreate saw %d items, want 1", len(items))
	}
	if items[0].FileName != name {
		t.Errorf("batchCreate simpleMediaItem.fileName = %q, want %q -- this is the field Google's reference says is shown to the user, and without it an uploaded photo cannot be traced back to its source file", items[0].FileName, name)
	}
	if header != name {
		t.Errorf("X-Goog-Upload-File-Name header = %q, want %q -- the documented fallback when fileName is absent", header, name)
	}
}

func TestUploadFileName_AndHeaderEncoding(t *testing.T) {
	longBase := strings.Repeat("a", 300)
	cases := []struct {
		name       string
		path       string
		wantName   string
		wantHeader string
	}{
		{"plain", "/lib/2024/IMG_0001.jpg", "IMG_0001.jpg", "IMG_0001.jpg"},
		{"spaces are kept readable", "/lib/My Photos/beach day.jpg", "beach day.jpg", "beach day.jpg"},
		{
			name: "non-ASCII survives in JSON, percent-encoded in the header",
			path: "/lib/vacaciones/España año.jpg", wantName: "España año.jpg",
			wantHeader: "Espa%C3%B1a a%C3%B1o.jpg",
		},
		{
			name: "CRLF cannot inject a header",
			path: "/lib/evil\r\nX-Injected: 1.jpg", wantName: "evil\r\nX-Injected: 1.jpg",
			wantHeader: "evil%0D%0AX-Injected: 1.jpg",
		},
		{"percent itself is escaped", "/lib/100%done.jpg", "100%done.jpg", "100%25done.jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uploadFileName(tc.path); got != tc.wantName {
				t.Errorf("uploadFileName(%q) = %q, want %q", tc.path, got, tc.wantName)
			}
			if got := headerSafeFileName(tc.path); got != tc.wantHeader {
				t.Errorf("headerSafeFileName(%q) = %q, want %q", tc.path, got, tc.wantHeader)
			}
		})
	}

	// Over-long names are capped at the documented 255, keeping the
	// extension, and never cut mid-rune.
	got := uploadFileName("/lib/" + longBase + ".jpg")
	if n := utf8.RuneCountInString(got); n > 255 {
		t.Errorf("long name came back %d runes, want <= 255", n)
	}
	if !strings.HasSuffix(got, ".jpg") {
		t.Errorf("long name lost its extension: %q", got)
	}
	multibyte := uploadFileName("/lib/" + strings.Repeat("ñ", 300) + ".jpg")
	if !utf8.ValidString(multibyte) {
		t.Errorf("capping a multi-byte name produced invalid UTF-8: %q", multibyte)
	}

	// A header value must always be valid to actually send, whatever the
	// filename. Proven by writing a real request rather than by
	// re-implementing Go's validity rules: net/http refuses to serialize a
	// request carrying an invalid header value, which is what a raw CRLF or
	// high byte in a filename would produce.
	for _, p := range []string{
		"/lib/España año.jpg",
		"/lib/evil\r\nX-Injected: 1.jpg",
		"/lib/" + strings.Repeat("ñ", 300) + ".jpg",
		"/lib/tab\there.jpg",
	} {
		req, err := http.NewRequest("POST", "https://example.invalid/v1/uploads", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Goog-Upload-File-Name", headerSafeFileName(p))
		if err := req.Write(io.Discard); err != nil {
			t.Errorf("a request naming %q could not be sent: %v", p, err)
		}
	}
}

// TestUploader_Run_SpaceSaverDoesNotLeakTheTempFileName guards the subtlety
// that makes this fix easy to get wrong: in space-saver mode the bytes come
// from a generated temp file, and naming the media item after THAT would be
// worse than sending no name at all -- the user would see
// "IMG_0001-3746192837.jpg" in Google Photos. The name must always come
// from the original source path.
func TestUploader_Run_SpaceSaverDoesNotLeakTheTempFileName(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	type simpleItem struct {
		UploadToken string `json:"uploadToken"`
		FileName    string `json:"fileName"`
	}
	var mu sync.Mutex
	var gotNames []string

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", srv.URL+"/upload-session")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/upload-session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "token-%d", len(body))
	})
	mux.HandleFunc("/v1/mediaItems:batchCreate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NewMediaItems []struct {
				SimpleMediaItem simpleItem `json:"simpleMediaItem"`
			} `json:"newMediaItems"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type resultItem struct {
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		}
		var results []resultItem
		mu.Lock()
		for _, it := range req.NewMediaItems {
			gotNames = append(gotNames, it.SimpleMediaItem.FileName)
			var ri resultItem
			ri.MediaItem.ID = "media-" + it.SimpleMediaItem.UploadToken
			results = append(results, ri)
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"newMediaItemResults": results})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	origUploadURL, origBatchURL := uploadURL, batchCreateURL
	uploadURL = srv.URL + "/v1/uploads"
	batchCreateURL = srv.URL + "/v1/mediaItems:batchCreate"
	defer func() { uploadURL, batchCreateURL = origUploadURL, origBatchURL }()

	db := openTestDB(t)
	dir := t.TempDir()
	const name = "IMG_0001.jpg"
	p := filepath.Join(dir, name)
	writeOversizedJPEG(t, p)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	mustDB(t, db.EnsurePending("hash-ss", info.Size(), "image/jpeg", p, nil))

	cfg := config.Defaults()
	cfg.UploadQuality = "space_saver"
	cfg.SpaceSaverMaxDim = 16 // force the downscale path
	u := &Uploader{
		client:      srv.Client(),
		db:          db,
		cfg:         cfg,
		dailyQuota:  quota.NewDailyQuota(db),
		concurrency: quota.NewAdaptiveConcurrency(6),
	}
	stats, err := u.Run()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", stats.Uploaded)
	}

	mu.Lock()
	names := append([]string(nil), gotNames...)
	mu.Unlock()
	if len(names) != 1 || names[0] != name {
		t.Errorf("fileName sent = %v, want [%s] -- space-saver's temp filename must never reach Google", names, name)
	}
}

// writeOversizedJPEG writes a real JPEG large enough to trigger the
// space-saver downscale.
func writeOversizedJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

// TestLinkToAlbum_ChunksAtGoogleCapAndDrainsBacklog covers a real,
// compounding bug. albums.batchAddMediaItems accepts at most 50 media
// items per call. linkToAlbum used to send every id it was given in ONE
// request, which is invisible on the normal path (batchCreate flushes at
// most 50, so it never saw more) but fatal on the retry path, where
// retryPendingAlbumLinks hands over EVERY outstanding link for a title at
// once. The first failure for an album therefore made the retry re-send
// the whole backlog in a single over-cap request, which was rejected,
// which re-recorded them all as failed -- a backlog that could only grow.
//

// TestRetryPendingAlbumLinks_SkippedWhenAlbumsAreOff proves that switching
// album_strategy to "none" actually stops the retry pass, rather than only
// stopping NEW links from being queued.
//
// The distinction is the whole point. This pass is wired into Run()
// unconditionally, so before this guard a user who had given up on albums
// still paid the full backlog cost on every single run forever: ~11 doomed
// requests and ~30 seconds of dead air before the first file moved, all
// spending the same contested write quota the circuit breaker exists to
// protect. Measured on a real library at exactly that -- 15,938 outstanding
// links, a run reporting "0/15983 files, 29s elapsed" while this ground
// through its budget.
//

// TestAlbumLinkNotices_OneLinePerRunNotPerRequest locks in the volume, not
// just the wording. The retry pass makes albumLinkRetryPerRun /
// albumLinkBatchMax requests per run -- eleven at the current constants --
// and reporting each chunk as it happened printed eleven identical lines
// every single run, forever, to say one thing. Reported exactly that way:
// "stop writing these errors to the screen... and the entire scrolling
// mechanism is now fucked up because of this."
//
