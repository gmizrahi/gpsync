package dashboard

import (
	"net/http"
	"testing"
)

func TestLoginAttemptTracker_LocksOutAfterMaxFailures(t *testing.T) {
	tr := newLoginAttemptTracker()
	if tr.locked("1.2.3.4") {
		t.Fatal("a fresh tracker must not report anyone locked out")
	}
	for i := 0; i < maxLoginFailures-1; i++ {
		tr.recordFailure("1.2.3.4")
		if tr.locked("1.2.3.4") {
			t.Fatalf("locked out after only %d failure(s), want %d", i+1, maxLoginFailures)
		}
	}
	tr.recordFailure("1.2.3.4")
	if !tr.locked("1.2.3.4") {
		t.Errorf("not locked out after %d failures, want locked", maxLoginFailures)
	}
}

func TestLoginAttemptTracker_DifferentIPsAreIndependent(t *testing.T) {
	tr := newLoginAttemptTracker()
	for i := 0; i < maxLoginFailures; i++ {
		tr.recordFailure("1.2.3.4")
	}
	if !tr.locked("1.2.3.4") {
		t.Fatal("setup: 1.2.3.4 should be locked out")
	}
	if tr.locked("5.6.7.8") {
		t.Error("a different IP's failures must not lock this one out")
	}
}

func TestLoginAttemptTracker_SuccessClearsFailureHistory(t *testing.T) {
	tr := newLoginAttemptTracker()
	for i := 0; i < maxLoginFailures-1; i++ {
		tr.recordFailure("1.2.3.4")
	}
	tr.recordSuccess("1.2.3.4")
	tr.recordFailure("1.2.3.4")
	if tr.locked("1.2.3.4") {
		t.Error("a successful login should have reset the failure count -- one more failure alone must not lock out")
	}
}

func TestHandler_Login_LocksOutAfterRepeatedFailures(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	var last *http.Response
	for i := 0; i < maxLoginFailures; i++ {
		resp := mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=wrong")
		resp.Body.Close()
		last = resp
	}
	if last.StatusCode != http.StatusOK {
		t.Fatalf("status after %d failures = %d, want 200 (still just re-rendering the form)", maxLoginFailures, last.StatusCode)
	}

	// The (maxLoginFailures+1)th attempt -- even with the CORRECT
	// password -- must be rejected by the lockout, not silently let
	// through.
	resp := mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=correct-horse")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status on the locked-out attempt = %d, want 429", resp.StatusCode)
	}

	// And no session was granted.
	pageResp := mustGet(t, client, srv.URL+"/settings")
	defer pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusSeeOther {
		t.Error("a locked-out login attempt must never grant a session, even with the right password")
	}
}
