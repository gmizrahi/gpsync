package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedUploaded records a file as really uploaded (with a media item id, so
// there is something to verify against).
func seedUploaded(t *testing.T, db interface {
	EnsurePending(string, int64, string, string, *float64) error
	MarkUploaded(string, string, string) error
}, sha, mediaID string) {
	t.Helper()
	mustNoErr(t, db.EnsurePending(sha, 10, "image/jpeg", "/lib/"+sha+".jpg", nil))
	mustNoErr(t, db.MarkUploaded(sha, mediaID, ""))
}

func TestVerifySample_ConfirmsPresentItems(t *testing.T) {
	db := openTestDB(t)
	seedUploaded(t, db, "h1", "media-1")
	seedUploaded(t, db, "h2", "media-2")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 2 || res.OK != 2 || len(res.Missing) != 0 {
		t.Errorf("result = %+v, want 2 checked / 2 ok / 0 missing", res)
	}
	verified, failed, total, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if verified != 2 || failed != 0 || total != 2 {
		t.Errorf("summary = %d verified / %d failed / %d total, want 2/0/2", verified, failed, total)
	}
}

// TestVerifySample_MissingItemIsReportedAndRecorded is the finding this
// whole feature exists to surface: the ledger says the file is uploaded
// and Google says it isn't there.
func TestVerifySample_MissingItemIsReportedAndRecorded(t *testing.T) {
	db := openTestDB(t)
	seedUploaded(t, db, "gone", "media-gone")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Media item not found."}}`))
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.OK != 0 {
		t.Fatalf("result = %+v, want exactly one missing item", res)
	}
	if res.Missing[0].SHA256 != "gone" {
		t.Errorf("missing item = %q, want \"gone\"", res.Missing[0].SHA256)
	}
	_, failed, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Errorf("failed count = %d, want 1 -- a miss has to persist, not just print once", failed)
	}
}

// TestVerifySample_TransientFailureIsInconclusiveNotMissing guards the
// most dangerous possible bug in a verifier: calling a backup lost
// because the network hiccuped. A dropped connection says nothing about
// whether the item exists, so it must not be recorded as a verdict at
// all -- the next pass has to retry it.
func TestVerifySample_TransientFailureIsInconclusiveNotMissing(t *testing.T) {
	db := openTestDB(t)
	seedUploaded(t, db, "h1", "media-1")

	// A server that is closed immediately: every request is a transport
	// error, exactly like an offline network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := srv.Client()
	MediaItemsURL = srv.URL + "/v1/mediaItems"
	srv.Close()

	res, err := VerifySample(client, db, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 0 {
		t.Errorf("a network failure was reported as a MISSING backup: %+v", res.Missing)
	}
	if res.Inconclusive != 1 {
		t.Errorf("Inconclusive = %d, want 1", res.Inconclusive)
	}
	// And crucially it must remain unverified, so the next pass retries it
	// rather than the blip counting as a check.
	verified, _, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if verified != 0 {
		t.Errorf("verified = %d, want 0 -- a transport error is not a verdict", verified)
	}
}

// TestVerifySample_AuthOrScopeErrorIsInconclusive covers the 403 case,
// which is the shape an API-restriction problem takes: it affects every
// item equally and is a gpsync bug to fix, not evidence that this one
// photo is gone.
func TestVerifySample_AuthOrScopeErrorIsInconclusive(t *testing.T) {
	db := openTestDB(t)
	seedUploaded(t, db, "h1", "media-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Request had insufficient authentication scopes."}}`))
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 0 {
		t.Errorf("a 403 must not be reported as a missing backup: %+v", res.Missing)
	}
	if res.Inconclusive != 1 {
		t.Errorf("Inconclusive = %d, want 1", res.Inconclusive)
	}
	// The reason is still recorded, so a systematic scope problem is
	// visible rather than silently retried forever.
	_, failed, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want the 403 reason recorded", failed)
	}
}

// TestVerifySample_RotatesThroughTheLibrary proves repeated sampling
// covers new ground instead of re-checking the same few items -- least-
// recently-verified first, never-verified before that.
func TestVerifySample_RotatesThroughTheLibrary(t *testing.T) {
	db := openTestDB(t)
	for _, sha := range []string{"a", "b", "c", "d"} {
		seedUploaded(t, db, sha, "media-"+sha)
	}

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		seen = append(seen, parts[len(parts)-1])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	now := time.Now()
	if _, err := VerifySample(srv.Client(), db, 2, now); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySample(srv.Client(), db, 2, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 4 {
		t.Fatalf("checked %d items across two passes, want 4", len(seen))
	}
	unique := map[string]bool{}
	for _, id := range seen {
		unique[id] = true
	}
	if len(unique) != 4 {
		t.Errorf("the second pass re-checked items the first already did: %v", seen)
	}
	verified, _, total, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if verified != total {
		t.Errorf("%d of %d verified after two passes, want full coverage", verified, total)
	}
}

// TestVerifySample_EverythingMissing_IsAnAccessProblemNotDataLoss guards
// the bug this feature shipped with. gpsync requests only
// photoslibrary.appendonly -- write-only -- and Google answers
// mediaItems.get on an unreadable item with 404, not 403. The first real
// run therefore reported 50 of 50 files as MISSING BACKUPS, including one
// the user was looking at in Google Photos at the time.
//
// Unanimity is the signal: when every single check says "gone", the
// likely explanation is that we cannot see anything, not that the library
// evaporated. It must be reported, but as an access problem, and no
// per-file verdict may be written -- a recorded "missing" here is false
// data about whether a backup exists.
func TestVerifySample_EverythingMissing_IsAnAccessProblemNotDataLoss(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < verifyAllMissingFloor+3; i++ {
		seedUploaded(t, db, fmt.Sprintf("h%d", i), fmt.Sprintf("media-%d", i))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Media item not found."}}`))
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 50, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.AccessProblem {
		t.Error("unanimous 404s must be reported as an access problem")
	}
	if len(res.Missing) != 0 {
		t.Errorf("no file may be reported as a missing backup here; got %d", len(res.Missing))
	}
	// And nothing may be written to the ledger -- that was the real damage.
	_, failed, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Errorf("%d false \"missing\" verdict(s) were recorded; want none", failed)
	}
}

// TestVerifySample_OneMissingAmongManyIsStillReported is the other side:
// the guard keys on unanimity, so a genuine single loss among healthy
// files must still surface. Otherwise the fix would suppress the only
// finding the feature exists for.
func TestVerifySample_OneMissingAmongManyIsStillReported(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 8; i++ {
		seedUploaded(t, db, fmt.Sprintf("h%d", i), fmt.Sprintf("media-%d", i))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "media-3") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 50, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessProblem {
		t.Error("one miss among many healthy files is not an access problem")
	}
	if len(res.Missing) != 1 || res.Missing[0].SHA256 != "h3" {
		t.Fatalf("want exactly h3 reported missing, got %+v", res.Missing)
	}
	_, failed, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Errorf("the genuine miss must be recorded; failed = %d", failed)
	}
}

// TestVerifySample_NoConfirmationsAmongMixedResults_IsStillAnAccessProblem
// covers the case the first version of the guard let through. It required
// EVERY check to be a 404; a real run produced 7 missing + 3 inconclusive
// out of 10, which isn't unanimous, so it sailed past and recorded seven
// more false "missing backup" verdicts.
//
// Inconclusive results are noise. The signal is that a random sample of
// files we believe we uploaded produced ZERO confirmations.
func TestVerifySample_NoConfirmationsAmongMixedResults_IsStillAnAccessProblem(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 10; i++ {
		seedUploaded(t, db, fmt.Sprintf("h%d", i), fmt.Sprintf("media-%d", i))
	}

	// Mixed failures, exactly like the reported run: mostly 404s with a
	// few that fail for unrelated reasons. Crucially, no successes.
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n%4 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	MediaItemsURL = srv.URL + "/v1/mediaItems"

	res, err := VerifySample(srv.Client(), db, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !res.AccessProblem {
		t.Error("zero confirmations across a whole sample must read as an access problem, even when some checks failed for other reasons")
	}
	if len(res.Missing) != 0 {
		t.Errorf("no file may be reported missing here; got %d", len(res.Missing))
	}
	// Nothing false may reach the ledger. The 500s legitimately record a
	// reason (they ARE inconclusive and worth surfacing), so the ledger
	// should hold exactly those and none of the 404s.
	_, failed, _, _, err := db.VerificationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if failed > 3 {
		t.Errorf("%d recorded problems; the 404s must not have been written as verdicts", failed)
	}
}
