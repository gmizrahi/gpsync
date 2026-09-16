package uploader

// ProgressEvent is reported after every file (success or failure) and after
// every wave, so the caller can show which file just finished, not just a
// bare counter.
type ProgressEvent struct {
	Done, Total int
	LastFile    string // path of the most recently completed file, "" between waves
	LastOK      bool
	LastSize    int64 // size in bytes of the file that just finished (0 for waves with no completion)
	// LastErrorMessage is why LastFile failed, when LastOK is false --
	// exactly the text just written to the ledger's last_error_message. Live
	// progress views showed only "failed" without it, so a folder where
	// dozens of files failed back-to-back gave the user nothing on screen to
	// act on; the reason existed only in the DB, reachable after the fact
	// via `gpsync log`. Always "" when LastOK is true.
	LastErrorMessage string
	// LastDeferred marks a file that was NOT finished: it hit a throttle and
	// has been set aside for the circuit breaker to retry after its pause
	// (see throttleCircuitBreakerSchedule). It is neither a success nor a
	// failure, and nothing has been written to the ledger for it -- a live
	// view should drop it from its in-flight list without counting it done,
	// or the file gets counted twice when the retry succeeds. Done is
	// deliberately unchanged from the previous event for these.
	LastDeferred bool
	// LastCancelled marks a file whose failure was a manual per-file skip
	// (uploader.FileCancelRegistry.Cancel, retryx.Cancelled), not a real
	// error -- LastOK is still false and it's still landed
	// failed_retryable in the ledger (see finalForThisFile), but a live
	// view showing it identically to a genuine failure is misleading, and
	// a skipped file was once labelled FAIL. Distinct from LastDeferred
	// (held for a later retry, nothing written to the ledger yet) --
	// this file's failure IS final, it just wasn't a failure the user
	// should read as something having gone wrong.
	LastCancelled bool
	// LastErrorKind is retryx.Classification.Kind (as a plain string, so
	// this package's own JSON/dashboard consumers don't need to import
	// retryx just to read it), empty when LastOK is true -- a dashboard
	// badge distinguishing "THROTTLED"/"QUOTA"/"FAILED" needs to know WHY a
	// file failed, not just that it did, so the badge can read THROTTLED
	// or SKIPPED rather than a bare FAILED
	// -- backed by the real classification rather than sniffing the error
	// text, which is what actually decided retry behavior for this file in
	// the first place.
	LastErrorKind string
}
