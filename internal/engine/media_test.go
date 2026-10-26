package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func testSHA(i int) string { return fmt.Sprintf("%064x", i) }

// addScannedMedia writes a file and records it the way a scan does: a
// files_seen row with its size and modification time, and a pending upload
// with its capture time. A .jpg gets real image data; anything else, bytes.
func addScannedMedia(t *testing.T, db *statedb.DB, sha, path string, captured time.Time) {
	t.Helper()
	if filepath.Ext(path) == ".jpg" {
		writeTestJPEG(t, path, 64, 48)
	} else if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileSeen(path, sha, float64(info.ModTime().UnixNano())/1e9, info.Size()); err != nil {
		t.Fatal(err)
	}
	at := unixSeconds(captured)
	if err := db.EnsurePending(sha, info.Size(), "", path, &at); err != nil {
		t.Fatal(err)
	}
}

// Issue #66: the list is in capture order, paged by a cursor that loses and
// repeats nothing -- including across two files captured in the same
// second -- and leaves out ignored, needs-review and confirmed-missing
// files and anything outside [from, to).
func TestListMedia_RangeOrderPagingAndExclusions(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	day := time.Date(2024, 5, 1, 0, 0, 0, 0, time.Local)
	var want []string
	for i, h := range []int{9, 10, 10, 11, 12} { // two at 10:00
		sha := testSHA(i + 1)
		addScannedMedia(t, db, sha, filepath.Join(dir, fmt.Sprintf("p%d.jpg", i)), day.Add(time.Duration(h)*time.Hour))
		want = append(want, sha)
	}
	video := testSHA(20)
	addScannedMedia(t, db, video, filepath.Join(dir, "v.mp4"), day.Add(13*time.Hour))
	want = append(want, video)

	addScannedMedia(t, db, testSHA(30), filepath.Join(dir, "before.jpg"), day.Add(-time.Hour))
	addScannedMedia(t, db, testSHA(31), filepath.Join(dir, "after.jpg"), day.Add(24*time.Hour))
	ignored, review, gone := testSHA(40), testSHA(41), testSHA(42)
	for _, sha := range []string{ignored, review} {
		at := unixSeconds(day.Add(14 * time.Hour))
		if err := db.EnsureNeedsReview(sha, 1, "", filepath.Join(dir, sha+".jpg"), &at); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ResolveNeedsReview(ignored, false); err != nil {
		t.Fatal(err)
	}
	addScannedMedia(t, db, gone, filepath.Join(dir, "gone.jpg"), day.Add(15*time.Hour))
	if err := db.MarkMissing(gone, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmMissing(gone, 2); err != nil {
		t.Fatal(err)
	}

	var got []MediaItem
	cursor, pages := "", 0
	for {
		items, next, err := ListMedia(db, MediaQuery{From: day, To: day.Add(24 * time.Hour), Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, items...)
		pages++
		if next == "" {
			break
		}
		if pages > 20 {
			t.Fatal("paging never ends")
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d files, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].SHA256 != want[i] {
			t.Errorf("position %d: %s, want %s", i, got[i].SHA256[60:], want[i][60:])
		}
	}
	if !got[0].CapturedAt.Equal(day.Add(9*time.Hour)) || got[0].Kind != "photo" || !got[0].HasThumb {
		t.Errorf("first item = %+v; want the 09:00 photo, with a thumbnail", got[0])
	}
	if last := got[len(got)-1]; last.Kind != "video" || last.HasThumb {
		t.Errorf("video item = %+v; want kind video and no thumbnail", last)
	}

	// No range is everything listable; a big page is one page.
	all, next, err := ListMedia(db, MediaQuery{})
	if err != nil || next != "" || len(all) != len(want)+2 {
		t.Errorf("unbounded list: %d items, next %q, err %v; want %d and no next page", len(all), next, err, len(want)+2)
	}
}

func TestListMedia_RefusesACursorItDidNotIssue(t *testing.T) {
	db := openTestDB(t)
	for _, c := range []string{"!!", "bm90LWEtY3Vyc29y", encodeMediaCursor(1, "short"), "TmFOfA"} {
		if _, _, err := ListMedia(db, MediaQuery{Cursor: c}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: err %v, want ErrBadCursor", c, err)
		}
	}
}

// A thumbnail is cached by content hash forever, so it must never be made
// from a file that no longer holds that content: an edited file is
// unavailable until a scan hashes it again -- unless an unchanged copy
// exists elsewhere. A file the list leaves out can't be fetched either.
func TestLocateMedia_OnlyAnUnchangedCopy(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	sha := testSHA(1)
	orig := filepath.Join(dir, "a.jpg")
	addScannedMedia(t, db, sha, orig, time.Now())

	if _, p, err := LocateMedia(db, sha); err != nil || p != orig {
		t.Fatalf("LocateMedia = %q, %v; want the original", p, err)
	}
	if err := os.WriteFile(orig, []byte("edited since the scan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LocateMedia(db, sha); !errors.Is(err, ErrMediaUnavailable) {
		t.Errorf("edited file: err %v, want ErrMediaUnavailable", err)
	}

	copyPath := filepath.Join(dir, "copy", "a.jpg")
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestJPEG(t, copyPath, 64, 48)
	info, _ := os.Stat(copyPath)
	if err := db.UpsertFileSeen(copyPath, sha, float64(info.ModTime().UnixNano())/1e9, info.Size()); err != nil {
		t.Fatal(err)
	}
	if _, p, err := LocateMedia(db, sha); err != nil || p != copyPath {
		t.Errorf("with an unchanged copy: %q, %v; want the copy", p, err)
	}

	if _, _, err := LocateMedia(db, testSHA(99)); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("unknown hash: err %v, want ErrMediaNotFound", err)
	}
	if err := db.MarkMissing(sha, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmMissing(sha, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LocateMedia(db, sha); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("confirmed missing: err %v, want ErrMediaNotFound", err)
	}
}

func TestSnapThumbSize(t *testing.T) {
	for in, want := range map[int]int{0: 256, 1: 256, 256: 256, 257: 512, 512: 512, 700: 1024, 5000: 1024} {
		if got := SnapThumbSize(in); got != want {
			t.Errorf("SnapThumbSize(%d) = %d, want %d", in, got, want)
		}
	}
}

// The cache serves a thumbnail it made without the original, survives many
// requests for the same one at once, refuses formats with no thumbnail, and
// stays under its bound by dropping the least recently used.
func TestThumbCache(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	c := NewThumbCache(filepath.Join(dir, "thumbs"), 1<<30)

	a := filepath.Join(src, "a.jpg")
	writeTestJPEG(t, a, 600, 400)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Thumbnail(testSHA(1), a, 256); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent request: %v", err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if data, err := c.Thumbnail(testSHA(1), a, 256); err != nil || len(data) == 0 {
		t.Errorf("cached thumbnail with the original gone: %d bytes, %v", len(data), err)
	}
	if _, err := c.Thumbnail(testSHA(2), filepath.Join(src, "v.mp4"), 256); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("video: err %v, want ErrNoThumbnail", err)
	}
	if _, err := c.Thumbnail(testSHA(3), filepath.Join(src, "absent.jpg"), 256); !errors.Is(err, ErrMediaUnavailable) {
		t.Errorf("unreadable original: err %v, want ErrMediaUnavailable", err)
	}

	// Bound: room for about three thumbnails of this size.
	one, _ := os.ReadFile(filepath.Join(dir, "thumbs", "256", testSHA(1)+".jpg"))
	small := NewThumbCache(filepath.Join(dir, "small"), int64(len(one))*7/2)
	base := time.Now().Add(-time.Hour)
	for i := 1; i <= 6; i++ {
		p := filepath.Join(src, fmt.Sprintf("%d.jpg", i))
		writeTestJPEG(t, p, 600, 400)
		if _, err := small.Thumbnail(testSHA(i), p, 256); err != nil {
			t.Fatal(err)
		}
		// Distinct, increasing recency, so "least recently used" is exact.
		f := filepath.Join(dir, "small", "256", testSHA(i)+".jpg")
		if _, err := os.Stat(f); err == nil {
			_ = os.Chtimes(f, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
		}
	}
	var total int64
	for _, e := range small.entries() {
		total += e.size
	}
	if total > small.maxBytes {
		t.Errorf("cache holds %d bytes, bound %d", total, small.maxBytes)
	}
	if _, err := os.Stat(filepath.Join(dir, "small", "256", testSHA(6)+".jpg")); err != nil {
		t.Errorf("the most recent thumbnail was evicted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small", "256", testSHA(1)+".jpg")); err == nil {
		t.Error("the least recently used thumbnail was kept")
	}
}
