package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// MediaItemsURL is the base for mediaItems.get. A package var so tests can
// point it at an httptest server, matching internal/uploader's own
// uploadURL/batchCreateURL pattern.
var MediaItemsURL = "https://photoslibrary.googleapis.com/v1/mediaItems"

// verifyAllMissingFloor is how many checks must agree before "everything
// is missing" is read as an access problem rather than as data loss. Low,
// because the signal is unanimity rather than volume.
const verifyAllMissingFloor = 5

// VerifyMiss is one item the ledger claims is uploaded but that Google
// would not confirm.
type VerifyMiss struct {
	SHA256 string
	Path   string
	Reason string
}

// VerifyResult reports one sampling pass.
type VerifyResult struct {
	Checked int
	OK      int
	// Missing is the finding that matters: the ledger says these exist and
	// Google says otherwise.
	Missing []VerifyMiss
	// AccessProblem means every single check came back 404, which points
	// at gpsync not being able to READ from Google at all rather than at
	// the files being gone -- see the guard in VerifySample.
	AccessProblem bool
	// Inconclusive covers checks that failed for reasons that say nothing
	// about whether the item exists -- a network error, a throttle, an
	// auth problem. Deliberately NOT counted as missing: reporting a
	// file as lost because the wifi dropped would be worse than useless.
	Inconclusive int
}

// VerifySample checks that a sample of items the ledger calls "uploaded"
// still actually exist in Google Photos, by asking for each one by the
// exact media item id already stored.
//
// This is the one thing a sync tool ought to do that this one never
// did: until now "uploaded" meant only that an API call once returned
// 200. The project has since had a concrete demonstration of why that
// isn't the same as true -- 16,222 album links sat recorded as
// "pending retry" while being permanently broken, and nothing noticed
// for months because nothing ever checked.
//
// Deliberately NOT the reconcile-against-the-API feature this project
// rejected long ago (see CLAUDE.md's "No reconcile-against-the-API step"
// rule). That one searched by filename and timestamp to decide whether to
// skip an upload, which is both slow and wrong. This asks a single exact
// question about an id we already hold, changes no upload decision, and
// samples rather than sweeping.
func VerifySample(client *http.Client, db *statedb.DB, sample int, now time.Time) (VerifyResult, error) {
	var res VerifyResult
	if sample <= 0 {
		return res, nil
	}
	candidates, err := db.UploadsToVerify(sample)
	if err != nil {
		return res, err
	}
	for _, c := range candidates {
		status, body, err := getMediaItem(client, c.MediaItemID)
		res.Checked++
		switch {
		case err != nil:
			// Says nothing about the item; don't record a verdict at all,
			// so the next pass retries this one rather than treating a
			// network blip as evidence.
			res.Inconclusive++
			continue
		case status == http.StatusOK:
			res.OK++
			if err := db.MarkVerified(c.SHA256, float64(now.Unix()), ""); err != nil {
				return res, err
			}
		case status == http.StatusNotFound:
			// Recorded as a candidate miss, NOT yet as a verdict -- see the
			// all-404 guard after the loop. A 404 is only evidence about
			// this item if we can read items at all; with an append-only
			// credential Google answers every mediaItems.get with 404, so
			// taken at face value it turns "no read access" into "your
			// files are gone".
			res.Missing = append(res.Missing, VerifyMiss{
				SHA256: c.SHA256, Path: c.Path,
				Reason: "not found in Google Photos (404) -- deleted there, or never really stored",
			})
		default:
			res.Inconclusive++
			// Record the reason but leave it counted as inconclusive: a
			// 403 almost certainly means an API-scope problem affecting
			// every item equally, which is a gpsync bug to fix, not
			// evidence that this particular photo is gone.
			if err := db.MarkVerified(c.SHA256, float64(now.Unix()), verifyFailure(status, body)); err != nil {
				return res, err
			}
		}
	}

	// ALL-404 GUARD, and not a hypothetical one. gpsync requests only
	// photoslibrary.appendonly -- a write-only scope -- and Google answers
	// mediaItems.get on an unreadable item with 404 rather than 403. The
	// first real run of this command therefore reported 50 of 50 files as
	// MISSING, including one the user could see in Google Photos
	// with full metadata while reading the output.
	//
	// Same shape as doctor's missing-source guard: when EVERYTHING comes
	// back missing, the overwhelmingly likelier explanation is that we
	// cannot see anything, not that the entire library evaporated. So the
	// finding is reported -- silence would be its own lie -- but as an
	// access problem, and no per-file verdict is written, because a
	// verdict of "missing" here would be false data in the ledger.
	//
	// Keyed on ZERO CONFIRMATIONS, not on every check being a 404. The
	// first version required unanimity across all checks, which a real run
	// promptly slipped past: 7 missing + 3 inconclusive out of 10 isn't
	// unanimous, so it recorded 7 more false verdicts. Inconclusive
	// results are noise here -- what actually distinguishes "we cannot
	// read" from "some files are gone" is that a random sample of files we
	// believe we uploaded produced NO successes at all.
	if res.OK == 0 && len(res.Missing) >= verifyAllMissingFloor {
		res.AccessProblem = true
		res.Inconclusive = res.Checked
		res.Missing = nil
		return res, nil
	}

	// Only now, having ruled that out, are the 404s worth recording.
	for _, m := range res.Missing {
		if err := db.MarkVerified(m.SHA256, float64(now.Unix()), m.Reason); err != nil {
			return res, err
		}
	}
	return res, nil
}

func getMediaItem(client *http.Client, id string) (int, []byte, error) {
	resp, err := client.Get(MediaItemsURL + "/" + id)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body, nil
}

// verifyFailure renders a non-200, non-404 response compactly enough to
// group on: an API
// error body is mostly boilerplate, and a thousand near-identical unique
// strings can't be counted.
func verifyFailure(status int, body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &parsed) == nil {
		msg = strings.TrimSpace(parsed.Error.Message)
	}
	if msg == "" {
		msg = strings.Join(strings.Fields(string(body)), " ")
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return fmt.Sprintf("verification failed: HTTP %d", status)
	}
	return fmt.Sprintf("verification failed: HTTP %d — %s", status, msg)
}
