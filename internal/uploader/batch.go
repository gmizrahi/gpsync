package uploader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// batchFlushCount and batchFlushInterval bound how long a byte-uploaded file
// waits before its batchCreate call (and therefore before it's marked
// uploaded and disappears from a live progress view): whichever comes
// first, so a handful of large/slow files never keep a large completed
// batch waiting, and a trickle of completions still gets committed
// reasonably promptly instead of accumulating forever.
//
// batchFlushCount is Google's own hard cap on newMediaItems per
// mediaItems.batchCreate call -- not a margin below it. batchCreate is the
// endpoint that actually gets throttled in practice (see
// throttleCircuitBreakerSchedule's comment), so fewer, larger calls for the
// same file count directly reduces how often this pipeline knocks on that
// specific rate limit.
//
// batchFlushInterval used to be 1 second -- deliberately short, for live
// progress freshness. Direct user research into Google's own documented
// upload guidance changed that calculus: upload tokens stay valid 24
// HOURS, so there is no protocol-level urgency to flush quickly at all,
// and every flush this interval forces before batchFlushCount is reached
// is a smaller, LESS quota-efficient batchCreate call than necessary --
// Google's own guidance is "accumulate 50, then commit once". At the old
// 1s interval, realistic upload throughput rarely accumulates anywhere
// close to 50 items before the ticker fires, so most real runs were
// sending far more, far smaller batchCreate calls than needed -- each one
// its own hit against the same undocumented "concurrent write request"
// quota this project has fought all session. 30s trades a little live-
// progress freshness (a file may take up to 30s longer to show as
// "uploaded" if nothing else forces an earlier flush) for a real chance at
// actually reaching close to the 50-item cap under normal throughput. A
// package var, not a const, so tests can shorten it (same pattern as
// throttleCircuitBreakerSchedule/backoffTickInterval above).
const batchFlushCount = 50

var batchFlushInterval = 30 * time.Second

// batchResult reports what one batchCreate cycle did.
type batchResult struct {
	outcomes []fileOutcome
	// throttled is non-nil when Google's write quota stopped this batch.
	// The rows in deferred were deliberately NOT recorded as failures: they
	// go back in the queue for the circuit breaker to retry after its
	// pause, exactly like a byte upload that gets throttled.
	throttled *retryx.Classification
	deferred  []statedb.Upload
}

// batchCreateAndLink registers a batch of uploaded bytes as media items and
// creates the media items in Google Photos.
//
// mediaItems.batchCreate is the call that actually spends the 'concurrent
// write request' quota -- the raw byte-upload endpoint is a different
// service with its own budget -- so in real use this, not uploadOneBytes,
// is where a throttle lands. It must therefore feed the same run-wide
// circuit breaker: a throttle here used to mark the whole batch
// failed_retryable and move on, which is how a 20-folder sync could burn
// through a sustained rate limit in 37 seconds, writing off ~20 files per
// batch and never once backing off or even printing a throttle line.
//
// Deferred rows are re-uploaded from scratch on the retry rather than
// having their upload tokens reused. Tokens are short-lived and the retry
// may be up to an hour later (the schedule's final rung); re-sending the
// bytes costs bandwidth but not Library API quota (raw uploads aren't
// metered against the daily ceiling -- see the note in
// uploadBytesAttempt), which is the budget that is actually under
// pressure here.
func (u *Uploader) batchCreateAndLink(successes []uploadSuccess, stats *Stats) batchResult {
	var res batchResult
	var outcomes []fileOutcome
	// fileName is what Google Photos actually shows for the item. Per the
	// batchCreate reference: "File name with extension of the media item.
	// This is shown to the user in Google Photos. The file name specified
	// during the byte upload process is ignored if this field is set."
	// Omitting it left uploaded photos with no usable name, so there was no
	// way to tell which local file one had come from.
	//
	// It is taken from FirstSourcePath, the ORIGINAL file -- deliberately
	// not from whatever path supplied the bytes, which in space-saver mode
	// is a temp file with a generated name.
	type simpleMediaItem struct {
		UploadToken string `json:"uploadToken"`
		FileName    string `json:"fileName,omitempty"`
	}
	type newMediaItem struct {
		SimpleMediaItem simpleMediaItem `json:"simpleMediaItem"`
	}
	items := make([]newMediaItem, len(successes))
	for i, s := range successes {
		items[i] = newMediaItem{SimpleMediaItem: simpleMediaItem{
			UploadToken: s.token,
			FileName:    uploadFileName(s.row.FirstSourcePath),
		}}
	}
	reqBody, _ := json.Marshal(map[string]any{"newMediaItems": items})

	resp, err := u.client.Post(batchCreateURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		// A network-level failure here (most commonly a client-side
		// timeout -- see baseHTTPClient's ResponseHeaderTimeout, 30s,
		// which a big batch can genuinely exceed if Google is slow to
		// process 50 items at once) used to be treated as an immediate,
		// no-backoff failure for every file in the batch: exactly the
		// bug this whole function's doc comment already describes fixing
		// for an explicit HTTP throttle response, just left unfixed on
		// THIS branch. It showed up badly once the batch window was
		// widened to accumulate closer to Google's 50-item cap:
		// bigger batches take longer to process, making a timeout more
		// likely, which then hammered the very next batch again
		// immediately with zero pause, over and over, regardless of how
		// long the account was left to rest -- a client-side bug, not
		// something that ever needed Google's own state to recover from.
		// Treated identically to an explicit Throttle response below:
		// held, not failed, so the run-wide breaker actually backs off
		// instead of retrying at full speed into whatever just failed.
		cls := retryx.Classification{Kind: retryx.Throttle, Message: err.Error()}
		res.throttled = &cls
		for _, s := range successes {
			res.deferred = append(res.deferred, s.row)
		}
		return res
	}
	u.dbErr("recording daily quota usage", u.dailyQuota.Record(1))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		// reclassifyDailyQuota applies here too, not just on the byte-upload
		// path: the same ambiguous "daily" wording can arrive on this
		// endpoint, and without it a burst throttle would abandon the run.
		cls := u.reclassifyDailyQuota(classifyHTTPError(resp.StatusCode, body, resp.Header.Get("Retry-After")))

		if cls.Kind == retryx.Throttle {
			// Held, not failed -- the circuit breaker owns this. Nothing is
			// written to the ledger, so these rows stay `pending` and are
			// simply retried after the pause.
			res.throttled = &cls
			for _, s := range successes {
				res.deferred = append(res.deferred, s.row)
			}
			return res
		}

		if cls.Kind == retryx.DailyQuota {
			// Corroborated daily exhaustion: the whole run is over, and it
			// must say so rather than silently writing off this batch.
			u.quotaHitThisRun.Store(true)
			if u.onThrottle != nil {
				used, _ := u.dailyQuota.Used()
				u.onThrottle(fmt.Sprintf("daily quota exceeded (confirmed by local count: %d used today) — %s", used, cls.Message), 0, 0)
			}
			for _, s := range successes {
				u.dbErr("recording daily-quota failure for "+s.row.FirstSourcePath,
					u.db.MarkFailed(s.row.SHA256, false, "DAILY_QUOTA_EXCEEDED", cls.Message))
				stats.FailedRetryable++
				outcomes = append(outcomes, fileOutcome{path: s.row.FirstSourcePath, ok: false, size: s.row.Size, errorMessage: cls.Message, errorKind: string(retryx.DailyQuota)})
			}
			res.outcomes = outcomes
			return res
		}

		permanent := cls.Kind == retryx.Permanent
		for _, s := range successes {
			u.dbErr("recording batchCreate failure for "+s.row.FirstSourcePath,
				u.db.MarkFailed(s.row.SHA256, permanent, "BATCH_CREATE_FAILED", cls.Message))
			if permanent {
				stats.FailedPermanent++
			} else {
				stats.FailedRetryable++
			}
			outcomes = append(outcomes, fileOutcome{path: s.row.FirstSourcePath, ok: false, size: s.row.Size, errorMessage: cls.Message, errorKind: string(cls.Kind)})
		}
		res.outcomes = outcomes
		return res
	}

	var result struct {
		NewMediaItemResults []struct {
			Status struct {
				Message string `json:"message"`
			} `json:"status"`
			MediaItem struct {
				ID string `json:"id"`
			} `json:"mediaItem"`
		} `json:"newMediaItemResults"`
	}
	_ = json.Unmarshal(body, &result)

	for i, s := range successes {
		if i >= len(result.NewMediaItemResults) {
			const msg = "no result returned for this item"
			u.dbErr("recording missing batchCreate result for "+s.row.FirstSourcePath,
				u.db.MarkFailed(s.row.SHA256, false, "BATCH_CREATE_ITEM_MISSING", msg))
			stats.FailedRetryable++
			outcomes = append(outcomes, fileOutcome{path: s.row.FirstSourcePath, ok: false, size: s.row.Size, errorMessage: msg, errorKind: string(retryx.Transient)})
			continue
		}
		r := result.NewMediaItemResults[i]
		if r.MediaItem.ID != "" {
			// The one write that absolutely must not fail silently: Google
			// already has these bytes. A dropped MarkUploaded leaves the row
			// `pending` and the next run re-uploads a file that's already
			// safely stored.
			u.dbErr("marking "+s.row.FirstSourcePath+" uploaded (Google already has this file -- it will be re-uploaded next run if this is not fixed)",
				u.db.MarkUploaded(s.row.SHA256, r.MediaItem.ID, statedb.QualityFor(u.cfg.UploadQuality)))
			stats.Uploaded++
			outcomes = append(outcomes, fileOutcome{path: s.row.FirstSourcePath, ok: true, size: s.row.Size})
		} else {
			msg := r.Status.Message
			if msg == "" {
				msg = "batchCreate item failed"
			}
			u.dbErr("recording batchCreate item failure for "+s.row.FirstSourcePath,
				u.db.MarkFailed(s.row.SHA256, false, "BATCH_CREATE_ITEM_FAILED", msg))
			stats.FailedRetryable++
			outcomes = append(outcomes, fileOutcome{path: s.row.FirstSourcePath, ok: false, size: s.row.Size, errorMessage: msg, errorKind: string(retryx.Transient)})
		}
	}

	res.outcomes = outcomes
	return res
}

// ── shared error classification from an HTTP response body ────────────
