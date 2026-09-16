package uploader

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/gmizrahi/gpsync/internal/preprocess"
	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// InFlightFile is one file's live upload progress: how many bytes have been
// sent of how many total. It lives here, beside the progress events it is
// derived from, so that consumers (the tray controller, the web dashboard)
// share one definition without depending on each other.
type InFlightFile struct {
	Sent, Total int64
}

// ByteProgressEvent reports incremental upload progress for one in-flight
// file's byte transfer (the finalize POST body), fired repeatedly as bytes
// are actually written to the network -- for a live "N% / size, speed" view
// per file, the way rclone's -P shows it, not just a per-file done/not-done
// state.
type ByteProgressEvent struct {
	Path      string
	SentDelta int64 // bytes sent since the last event for this path (for computing instantaneous rate)
	Sent      int64 // cumulative bytes sent for this path
	Total     int64 // total size of this file
}

// uploadBytesAttempt performs one attempt at uploading a file's bytes.
// retry reports whether the caller should try again: false means the
// returned (token, cls) pair is final -- either a success (token set, cls
// nil) or a failure not worth retrying (Permanent/DailyQuota).
//
// Split out of the retry loop specifically so the space-saver temp file
// gets a `defer cleanup()` that fires on every exit path. Inline in the
// loop, with its many `continue`s, each temp copy simply leaked into
// TempDir forever.
func (u *Uploader) uploadBytesAttempt(row statedb.Upload) (string, *retryx.Classification, bool) {
	uploadPath, cleanup := preprocess.PrepareUploadPath(row.FirstSourcePath, u.cfg)
	defer cleanup()

	// fileCtx bounds THIS file's own HTTP requests, but deliberately does
	// NOT derive from u.ctx: Run()/Stop() already lets whatever's actively
	// transferring finish normally when the whole run is cancelled (see
	// Stop()'s doc comment in cmd/gpsync-tray/main.go -- "no new uploads
	// start, nothing in flight is cut off"), and a child of u.ctx would
	// cancel every in-flight file the instant the run context is
	// cancelled, silently breaking that. fileCtx is cancelled only by an
	// explicit FileCancelRegistry.Cancel(row.FirstSourcePath) call, or by
	// this function's own cleanup once the attempt is done. Registered for
	// the duration of this one attempt only: a cancelled attempt never
	// retries anyway (see finalForThisFile), so there's nothing to leave
	// registered past this function returning.
	fileCtx, fileCancel := context.WithCancel(context.Background())
	if u.fileCancels != nil {
		u.fileCancels.register(row.FirstSourcePath, fileCancel)
		defer u.fileCancels.unregister(row.FirstSourcePath)
	}
	defer fileCancel()

	mime := scanner.GuessMime(uploadPath)
	info, statErr := os.Stat(uploadPath)
	if statErr != nil {
		return "", &retryx.Classification{Kind: retryx.Transient, Message: statErr.Error()}, true
	}

	startReq, err := http.NewRequestWithContext(fileCtx, "POST", uploadURL, nil)
	if err != nil {
		return "", &retryx.Classification{Kind: retryx.Permanent, Message: err.Error()}, false
	}
	startReq.Header.Set("Content-Length", "0")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Content-Type", mime)
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Raw-Size", strconv.FormatInt(info.Size(), 10))
	// Belt and braces alongside batchCreate's fileName (which overrides
	// this when set, and is the field the docs say is displayed). Not in
	// the Photos API's documented start-header list, but it is the
	// "file name specified during the byte upload process" that the
	// batchCreate reference refers to, and costs nothing to send.
	//
	// From the ORIGINAL path, not uploadPath: in space-saver mode the bytes
	// come from a temp file whose generated name must never reach Google.
	if name := headerSafeFileName(row.FirstSourcePath); name != "" {
		startReq.Header.Set("X-Goog-Upload-File-Name", name)
	}

	startResp, err := u.client.Do(startReq)
	if err != nil {
		if fileCtx.Err() != nil {
			return "", &retryx.Classification{Kind: retryx.Cancelled, Message: "upload skipped by request"}, false
		}
		return "", &retryx.Classification{Kind: retryx.Transient, Message: err.Error()}, true
	}
	// Not counted against the local daily-request tracker: the 10k/day
	// Library API quota is about structured API calls (batchCreate,
	// mediaItems.list, ...), not the raw upload-bytes endpoint
	// -- this project's own quota research already assumed as much (see
	// CLAUDE.md's "~500k media item registrations/day" figure, which
	// only holds if raw uploads aren't ALSO metered). Counting them here
	// used to inflate the local tracker well past Google's real usage
	// (confirmed against the actual Cloud Console figure), which made
	// Run()'s proactive dailyQuota.Exhausted() check trip early.

	if startResp.StatusCode != 200 {
		body, _ := io.ReadAll(startResp.Body)
		startResp.Body.Close()
		cls := u.reclassifyDailyQuota(classifyHTTPError(startResp.StatusCode, body, startResp.Header.Get("Retry-After")))
		return "", &cls, !finalForThisFile(cls.Kind)
	}
	uploadSessionURL := startResp.Header.Get("X-Goog-Upload-URL")
	startResp.Body.Close()
	if uploadSessionURL == "" {
		return "", &retryx.Classification{Kind: retryx.Transient, Message: "No upload URL returned by Google"}, true
	}

	f, err := os.Open(uploadPath)
	if err != nil {
		return "", &retryx.Classification{Kind: retryx.Transient, Message: err.Error()}, true
	}

	var reqBody io.Reader = f
	if u.onBytes != nil {
		sourcePath, total := row.FirstSourcePath, info.Size()
		var sent int64
		reqBody = &progressReader{r: f, onRead: func(n int64) {
			sent += n
			u.onBytes(ByteProgressEvent{Path: sourcePath, SentDelta: n, Sent: sent, Total: total})
		}}
	}

	finalizeReq, err := http.NewRequestWithContext(fileCtx, "POST", uploadSessionURL, reqBody)
	if err != nil {
		f.Close()
		return "", &retryx.Classification{Kind: retryx.Permanent, Message: err.Error()}, false
	}
	finalizeReq.ContentLength = info.Size()
	finalizeReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	finalizeReq.Header.Set("X-Goog-Upload-Offset", "0")

	finalizeResp, err := u.client.Do(finalizeReq)
	f.Close()
	if err != nil {
		// This is the request that's actually IN FLIGHT for the whole
		// file transfer, so a "skip this file" click almost always lands
		// here, not on the (fast) start request above.
		if fileCtx.Err() != nil {
			return "", &retryx.Classification{Kind: retryx.Cancelled, Message: "upload skipped by request"}, false
		}
		return "", &retryx.Classification{Kind: retryx.Transient, Message: err.Error()}, true
	}
	// Same reasoning as the start-response above: this is the raw
	// byte-upload call, not a structured Library API call.

	body, _ := io.ReadAll(finalizeResp.Body)
	finalizeResp.Body.Close()

	if finalizeResp.StatusCode != 200 {
		cls := u.reclassifyDailyQuota(classifyHTTPError(finalizeResp.StatusCode, body, finalizeResp.Header.Get("Retry-After")))
		return "", &cls, !finalForThisFile(cls.Kind)
	}

	return string(body), nil, false
}

// ── phase 2: serialized batchCreate ───────────────────────────────────
