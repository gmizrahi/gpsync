package dashboard

import (
	"math"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// openTestDB opens a fresh, isolated ledger for one test -- same pattern
// internal/engine's own tests use (see internal/engine/pending_test.go).
func openTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	// config.ConfigPath is computed once from statedb.StateDir at package
	// init time, not re-read per call -- same fix internal/backup's own
	// tests already need (see internal/backup/backup_test.go).
	config.ConfigPath = filepath.Join(dir, "config.toml")
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeController is a hand-written Controller test double -- not a mock
// framework, matching this project's existing style. Every field a test
// cares about is exported so a test can set it directly before serving a
// request, and running/started/stopped/retryRequested/cancelledPath/
// cfgSet record what handlers actually did, for tests that assert on
// mutation rather than just the HTTP response.
type fakeController struct {
	cfg      config.Config
	running  bool
	scanning bool

	fileUploaded           int
	fileUploading          int
	fileTotal              int
	folderIdx, folderTotal int
	currentFolder          string
	bytesDone, bytesTotal  int64
	runStart               time.Time
	runPaused              time.Duration
	inFlight               map[string]uploader.InFlightFile
	recent                 []uploader.ProgressEvent
	backoff                *uploader.BackoffStatus

	started, stopped bool
	retryRequested   bool
	cancelledPath    string
	cancelResult     bool
	setConfigCalls   []config.Config
}

func (f *fakeController) Config() config.Config { return f.cfg }
func (f *fakeController) SetConfig(cfg config.Config) {
	f.cfg = cfg
	f.setConfigCalls = append(f.setConfigCalls, cfg)
}
func (f *fakeController) IsRunning() bool { return f.running }
func (f *fakeController) Start()          { f.started = true; f.running = true }
func (f *fakeController) Stop()           { f.stopped = true; f.running = false }
func (f *fakeController) Scanning() bool  { return f.scanning }
func (f *fakeController) FileProgress() (int, int, int) {
	return f.fileUploaded, f.fileUploading, f.fileTotal
}
func (f *fakeController) FolderProgress() (int, int) { return f.folderIdx, f.folderTotal }
func (f *fakeController) CurrentFolder() string      { return f.currentFolder }
func (f *fakeController) RunProgress() (int64, int64, time.Time) {
	return f.bytesDone, f.bytesTotal, f.runStart
}
func (f *fakeController) RunPausedFor() time.Duration                     { return f.runPaused }
func (f *fakeController) InFlightFiles() map[string]uploader.InFlightFile { return f.inFlight }
func (f *fakeController) RecentEvents() []uploader.ProgressEvent          { return f.recent }
func (f *fakeController) CurrentBackoff() *uploader.BackoffStatus         { return f.backoff }
func (f *fakeController) RequestRetryNow()                                { f.retryRequested = true }
func (f *fakeController) CancelFile(path string) bool {
	f.cancelledPath = path
	return f.cancelResult
}

// newFakeController returns one with a usable default config -- a fresh
// statedb.DB has no config.toml, so wc.Config() in real code always
// returns config.Defaults() until something calls SetConfig; the fake
// mirrors that.
func newFakeController() *fakeController {
	return &fakeController{cfg: config.Defaults()}
}

// newTestServer builds a real httptest.Server around Handler(db, ctrl,
// opts) -- used by every test that needs to exercise cookies/redirects
// end-to-end (auth, CSRF) rather than a bare httptest.NewRecorder.
func newTestServer(t *testing.T, db *statedb.DB, ctrl Controller, opts Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(db, ctrl, opts))
	t.Cleanup(srv.Close)
	return srv
}

// newTestClient returns an http.Client with a cookie jar (so a session
// cookie from /login is remembered across requests, like a real browser)
// and that does NOT follow redirects automatically -- most assertions in
// this file want to see the 303 itself, not silently land on /login.
func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// mustGet/mustPost are small helpers to cut boilerplate across the tests
// below -- both fail the test on a transport-level error (a bad URL, a
// connection refused), never on a non-2xx status, since asserting on the
// actual status code is what most of these tests are for.
func mustGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustPostForm(t *testing.T, client *http.Client, url, origin string, form string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Speed and ETA are computed over transfer time: elapsed minus throttle
// backoff, which otherwise reported 38 KB/s and a 225h ETA after five
// backoff rungs.
func TestBuildStatus_TransferSecsExcludesBackoff(t *testing.T) {
	db := openTestDB(t)
	f := newFakeController()
	f.bytesDone, f.bytesTotal = 100<<20, 1000<<20
	f.runStart = time.Now().Add(-80 * time.Minute)
	f.runPaused = 70 * time.Minute

	resp, err := buildStatus(db, f)
	if err != nil {
		t.Fatal(err)
	}
	if resp.RunElapsedSecs < 79*60 {
		t.Errorf("RunElapsedSecs = %.0f, want wall-clock elapsed (~4800)", resp.RunElapsedSecs)
	}
	if resp.TransferSecs < 9*60 || resp.TransferSecs > 11*60 {
		t.Errorf("TransferSecs = %.0f, want elapsed minus backoff (~600)", resp.TransferSecs)
	}
}

// Media-type bar percentages always add up to exactly 100%. They once read
// "100%" beside "0%" with 680MB of 285.7GB pending, then "99.7%" beside
// "0.2%" when ignored files were counted in the denominator.
func TestPairPct(t *testing.T) {
	const gb, mb = int64(1) << 30, int64(1) << 20
	cases := []struct {
		done, pending         int64
		wantDone, wantPending string
	}{
		{2847 * gb / 10, 6802 * mb / 10, "99.8%", "0.2%"}, // the reported Photos bar
		{5896 * gb / 10, 209 * gb / 10, "96.6%", "3.4%"},
		{100, 0, "100%", "0%"},
		{0, 100, "0%", "100%"},
		{9_995, 5, "99.95%", "0.05%"},
		{1_000_000_000, 1, "99.99%", "0.01%"}, // tiny, but never 0%
		{9_996, 4, "99.96%", "0.04%"},
		{1, 1, "50.0%", "50.0%"},
		{27, 973, "2.7%", "97.3%"},
	}
	for _, c := range cases {
		d, p := pairPct(c.done, c.pending)
		if d != c.wantDone || p != c.wantPending {
			t.Errorf("pairPct(%d, %d) = %q, %q; want %q, %q", c.done, c.pending, d, p, c.wantDone, c.wantPending)
		}
	}

	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			t.Fatalf("unparseable percentage %q", s)
		}
		return v
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		done := rng.Int63n(1 << uint(rng.Intn(42)+1))
		pending := rng.Int63n(1 << uint(rng.Intn(42)+1))
		if done == 0 && pending == 0 {
			continue // no segments: the bar is not drawn at all
		}
		d, p := pairPct(done, pending)
		if sum := parse(d) + parse(p); math.Abs(sum-100) > 1e-9 {
			t.Fatalf("pairPct(%d, %d) = %q + %q = %v, want exactly 100", done, pending, d, p, sum)
		}
		if pending > 0 && done > 0 && (parse(p) == 0 || parse(d) == 100) {
			t.Fatalf("pairPct(%d, %d) = %q, %q hides one side", done, pending, d, p)
		}
	}
}

// Retryable files are still to upload: Total Size once read "Pending 0 B"
// beside 103 retryable files (11.6 GB) after a throttle give-up.
func TestBuildStatus_ReportsRetryableBytes(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsurePending("h1", 11_600, "video/mp4", "/lib/clip.mp4", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkFailed("h1", false, "THROTTLE_BACKOFF_EXHAUSTED", "gave up"); err != nil {
		t.Fatal(err)
	}
	resp, err := buildStatus(db, newFakeController())
	if err != nil {
		t.Fatal(err)
	}
	if resp.RetryableBytes != 11_600 || resp.PendingBytes != 0 {
		t.Errorf("RetryableBytes=%d PendingBytes=%d, want 11600 and 0", resp.RetryableBytes, resp.PendingBytes)
	}
}
