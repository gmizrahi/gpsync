package dashboard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatusPageJS_RefreshDoesNotThrow actually EXECUTES statusPageBody's
// embedded <script> under Node, driving refresh() against a handful of
// realistic /api/status payloads. Every other test in this file only
// checks CSS/JS as STRINGS (does a substring appear, does a rule close) --
// none of them can catch a real JS runtime bug, which is exactly what
// slipped through here: live-foot's footer code referenced `rows` and
// `uploadingCapacity`, both declared with `const` INSIDE the "files are
// actively uploading" if-block, from code sitting AFTER that if/else --
// a plain ReferenceError, thrown on literally every single poll,
// regardless of branch. It was invisible to `go build`/`go vet`/`go
// test` (this is all embedded string content to Go) and even invisible
// to a real user staring at the Network tab, since the underlying fetch
// to /api/status succeeded with well-formed JSON every time -- the
// exception happened AFTER parsing, inside refresh()'s own try block,
// so its outer catch overwrote the page with a generic "dashboard
// server unreachable" banner that had nothing to do with the real
// cause. Skips (doesn't fail) if `node` isn't on PATH, since this is the
// one test in this package with an external tool dependency.
func TestStatusPageJS_RefreshDoesNotThrow(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH -- skipping JS execution smoke test")
	}

	i := strings.Index(statusPageBody, "<script>")
	if i < 0 {
		t.Fatal("no <script> tag found in statusPageBody")
	}
	j := strings.Index(statusPageBody[i:], "</script>")
	if j < 0 {
		t.Fatal("<script> tag never closes in statusPageBody")
	}
	script := statusPageBody[i+len("<script>") : i+j]
	// Strip the script's own trailing self-invocation (a top-level
	// "refresh();" followed by the setInterval poll) -- it would
	// otherwise fire an UNCONTROLLED extra refresh() call, racing against
	// this test's own deliberate, sequenced calls below, the moment this
	// file finishes hoisting its function declarations and using whatever
	// global.fetch happens to be set to at that instant. Anchored on the
	// unindented top-level call rather than on the setInterval line, so
	// changing the poll interval (or commenting it) doesn't break this.
	if k := strings.LastIndex(script, "\nrefresh();\n"); k >= 0 {
		script = script[:k]
	} else {
		t.Fatal("expected statusPageBody's script to end with a top-level \"refresh();\" call -- update this test if that trailing invocation changed shape")
	}

	// Minimal stubs: getElementById returns a STATEFUL per-id object (a
	// real element would be, effectively) so this test can inspect what
	// refresh() actually wrote, not just whether it threw -- refresh()'s
	// own try/catch (the ENTIRE point of this test: see the doc comment
	// above) swallows any internal error itself and never lets its
	// promise reject, so "did refresh() throw" is exactly the wrong
	// question; "did it silently fall back to the generic error banner"
	// is the real, observable symptom this test needs to catch.
	// querySelector stays null -- matches a real cold first-ever poll,
	// before any row has ever rendered, which measureUploadingCapacity/
	// measureRecentCapacity already fall back to a fixed constant for.
	harness := `
const elements = {};
// A .live-list stand-in with REAL geometry, so measureListCapacity runs
// its actual pitch measurement (rows[1].offsetTop - rows[0].offsetTop)
// instead of bailing to its fallback and leaving that logic untested.
// 16px rows on a 21px pitch reproduces the real page's collapsed 0.3rem
// margins; 560px of room therefore fits 26 rows, not the 21 the old
// "offsetHeight + marginTop + marginBottom" model would have claimed.
const rowGeometry = [];
for (let i = 0; i < 40; i++) rowGeometry.push({ offsetHeight: 16, offsetTop: i * 21 });
const fakeList = {
  clientHeight: 560,
  querySelectorAll: () => rowGeometry,
  querySelector: () => rowGeometry[0],
};
global.document = {
  getElementById: (id) => { if (!elements[id]) elements[id] = { innerHTML: '' }; return elements[id]; },
  querySelector: (sel) => (sel && sel.indexOf('.live-list') !== -1 ? fakeList : null),
};
global.getComputedStyle = () => ({ marginTop: '4.8px', marginBottom: '4.8px' });
global.setInterval = () => {};
// capacityCeiling reads window.innerHeight to bound the row-count
// measurement by what the viewport could physically show; stubbed so
// that path is actually exercised rather than silently falling back.
global.window = { innerHeight: 1200 };

function mkFile(i, pct) {
  return { path: 'C:/x/file' + i + '.jpg', folder: 'C:/x', name: 'file' + i + '.jpg', sent: pct * 1000, total: 100000, percent: pct };
}
function mkEvent(i, ok, cancelled, errorKind) {
  return { name: 'evt' + i + '.jpg', ok: ok, cancelled: cancelled, error: errorKind ? 'some error' : '', size: 12345, error_kind: errorKind || '' };
}
const inFlight = [];
for (let i = 0; i < 20; i++) inFlight.push(mkFile(i, i % 2 === 0 ? 100 : 40));
const recentEvents = [];
for (let i = 0; i < 25; i++) recentEvents.push(mkEvent(i, i % 4 !== 0, i % 7 === 0, i % 5 === 0 ? 'throttle' : ''));

// Three real states this bug actually manifested under: idle+scanning
// (in_flight/recent both empty -- the ORIGINAL reported screenshot),
// paused-with-a-real-payload (mirrors an actual /api/status response
// captured from the browser's own Network tab), and busy (nonempty
// in_flight/recent, exercising the ghost-row/column-split code paths).
const scenarios = {
  idleScanning: { watching: true, run_active: true, scanning: true, autostart_installed: true, current_folder: 'C:\\Photos\\2008', folder_index: 292, folder_total: 2410, synced: 100, pending: 5, failed_retryable: 0, failed_permanent: 0, synced_bytes: 1000, pending_bytes: 500, quota_used: 1, quota_daily_limit: 10000, uploaded_today: 1, uploaded_today_bytes: 100, in_flight: [], recent: [] },
  pausedRealData: { watching: true, run_active: true, autostart_installed: true, bytes_done: 0, bytes_total: 794405754, current_folder: 'C:\\Photos\\2018', failed_permanent: 0, failed_retryable: 0, files_total: 78, files_uploaded: 5, files_uploading: 3, folder_index: 1437, folder_total: 2410, pause_concurrency: 4, pause_max_concurrency: 6, pause_reason: 'Quota exceeded', pause_remaining_secs: 45, pause_rung: 2, pause_rung_wait_secs: 60, pause_total_rungs: 8, paused: true, pending: 22057, pending_bytes: 269231728049, quota_daily_limit: 10000, quota_used: 683, synced: 100, synced_bytes: 1000, in_flight: [], recent: [] },
  busy: { watching: true, run_active: true, files_uploaded: 5, files_uploading: 3, files_total: 71, current_folder: 'C:\\Photos\\2018', synced: 100, pending: 5, failed_retryable: 0, failed_permanent: 0, synced_bytes: 1000, pending_bytes: 500, quota_used: 1, quota_daily_limit: 10000, uploaded_today: 1, uploaded_today_bytes: 100, in_flight: inFlight, recent: recentEvents },
};

async function run(name) {
  global.fetch = async () => ({ ok: true, json: async () => scenarios[name] });
  elements['status-top'] = { innerHTML: '' };
  await refresh();
  // This is the actual symptom a real user hit: refresh()'s own try/catch
  // catches ANY internal error (a real fetch failure, OR a plain JS bug
  // like the ReferenceError this test exists to guard against) and
  // writes this exact banner to #status-top -- indistinguishable, from
  // the outside, from a genuine "the server is unreachable" failure.
  // For every one of these scenarios, the fetch succeeds and the JSON is
  // well-formed, so refresh() reaching this banner ALWAYS means a real,
  // internal JS bug, never an actual connectivity problem.
  if (elements['status-top'].innerHTML.includes('dashboard server unreachable')) {
    console.error('SCENARIO_FAILED:' + name + ': refresh() fell back to the generic error banner despite a successful, well-formed fetch response -- a real internal JS bug (e.g. a ReferenceError from a variable used out of its declared scope), not a genuine server-unreachable condition.');
    process.exit(1);
  }
}

// The capacity measurement decides how many rows each list renders, so
// getting it wrong leaves a permanently blank band at the bottom of a
// card that had content to put there -- reported many times, most
// concretely as "still do not use the entire length of the tile for
// listing files". With the geometry stubbed above (560px of room, 16px
// rows on a 21px pitch, one 5px trailing margin) exactly 26 rows fit:
// floor((560 - 5) / 21). The superseded box-model formula
// ("offsetHeight + marginTop + marginBottom" = 25.6px, ignoring that
// adjacent rows' margins COLLAPSE) would claim 21 -- five rows short,
// which is the bug this asserts against.
function checkCapacity(name, got) {
  if (got !== 26) {
    console.error('CAPACITY_WRONG:' + name + ': got ' + got + ', want 26 -- the row pitch must be MEASURED between two rendered rows, not computed from one row"s box model (adjacent vertical margins collapse, so summing marginTop+marginBottom overstates every row and leaves the list short).');
    process.exit(1);
  }
}

function fail(msg) { console.error(msg); process.exit(1); }

(async () => {
  for (const name of Object.keys(scenarios)) {
    await run(name);
  }
  checkCapacity('uploading', measureUploadingCapacity());
  checkCapacity('recent', measureRecentCapacity());

  // Phone layout: the list is sized by its CONTENT under a max-height
  // ceiling, so clientHeight is just the previous poll's rows. Measured on
  // a real 384px render: Recent activity at 54px holding 2 rows (20px
  // rows on a 25px pitch) under a 352px ceiling. Capacity used to come
  // out as floor((54 - 5) / 25) = 1, the list re-rendered one row, and it
  // stayed there -- five recent events on screen as one. With the ceiling
  // as the space to fill: floor((352 - 5) / 25) = 13.
  const phoneRows = [{ offsetHeight: 20, offsetTop: 0 }, { offsetHeight: 20, offsetTop: 25 }];
  const phoneList = { clientHeight: 54, querySelectorAll: () => phoneRows, querySelector: () => phoneRows[0] };
  const savedQS = global.document.querySelector, savedCS = global.getComputedStyle;
  global.document.querySelector = (sel) => (sel && sel.indexOf('.live-list') !== -1 ? phoneList : null);
  global.getComputedStyle = () => ({ marginTop: '4.8px', marginBottom: '4.8px', maxHeight: '352px' });
  const phoneCap = measureRecentCapacity();
  if (phoneCap !== 13) {
    fail('PHONE_CAPACITY_DECAY: got ' + phoneCap + ', want 13 -- a content-sized list under a max-height must be measured against that ceiling, or it loses a row every poll until it shows one');
  }
  global.document.querySelector = savedQS;
  global.getComputedStyle = savedCS;

  // Last message must be STICKY. The reported symptom: "don't reset the
  // last message to '---' when you retry uploads; Last message = last
  // message". Pressing Retry Now clears s.paused, and until some file
  // finishes, s.recent can be empty -- so the naive render (which had no
  // memory across polls) blanked the strip to a dash exactly when the
  // throttle reason it had been showing was the useful thing on screen.
  await run('busy');
  // Run shows uploaded, uploading and total separately: a combined
  // "uploaded + in flight" figure went down whenever a throttle failed the
  // files being sent.
  // Order: uploading, uploaded, total.
  const runRow = elements['status-top'].innerHTML;
  const iUploading = runRow.indexOf('3 uploading'), iUploaded = runRow.indexOf('5 uploaded'), iTotal = runRow.indexOf('71 total');
  if (iUploading < 0 || iUploaded < 0 || iTotal < 0 || !(iUploading < iUploaded && iUploaded < iTotal) || runRow.includes('Uploading ')) {
    fail('RUN_COUNTS: expected "3 uploading  •  5 uploaded  •  71 total" in that order in the Run row, got: ' + runRow);
  }
  const afterBusy = elements['status-lastmsg'].innerHTML;
  if (!afterBusy.includes('Uploaded') && !afterBusy.includes('Skipped')) {
    fail('FOOTER_EMPTY: expected a real Last message after the busy scenario, got: ' + afterBusy);
  }
  // A poll with nothing to say: not paused, no recent events. This is the
  // post-Retry-Now state.
  await run('idleScanning');
  const afterQuiet = elements['status-lastmsg'].innerHTML;
  if (afterQuiet !== afterBusy) {
    fail('FOOTER_RESET: a poll with no pause reason and no recent events overwrote Last message.\n  before: ' + afterBusy + '\n  after:  ' + afterQuiet);
  }

  // Last successful upload: renders "<Month> <day>, <year> hh:mm:ss  ·
  // Hh Mm Ss ago" from the raw epoch, and a dash when the ledger has
  // never recorded one (rather than "January 1, 1970").
  const withUpload = Object.assign({}, scenarios.busy, { last_upload_at: (Date.now() / 1000) - 7385 });
  global.fetch = async () => ({ ok: true, json: async () => withUpload });
  await refresh();
  const up = elements['status-lastupload'].innerHTML;
  if (!/(January|February|March|April|May|June|July|August|September|October|November|December) \d{1,2}, \d{4} \d{2}:\d{2}:\d{2}/.test(up)) {
    fail('STAMP_WRONG: expected a "<Month> <day>, <year> hh:mm:ss" stamp, got: ' + up);
  }
  // 7385s = 2h 3m 5s. The hours unit is the point: fmtDuration used to
  // roll hours into minutes, which would render this as "124m 5s".
  if (!/2h 3m \ds ago/.test(up)) {
    fail('AGO_WRONG: expected "2h 3m Ns ago" (hours must not be rolled into minutes), got: ' + up);
  }
  await run('idleScanning');
  if (!elements['status-lastupload'].innerHTML.includes('—')) {
    fail('NEVER_UPLOADED_WRONG: with no last_upload_at the cell must read as a dash, got: ' + elements['status-lastupload'].innerHTML);
  }

  console.log('ALL_SCENARIOS_OK');
  process.exit(0);
})();
`

	full := harness + script

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "refresh_smoke.js")
	if err := os.WriteFile(scriptPath, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}

	// A hard timeout, not just relying on setInterval being stubbed --
	// belt and suspenders against this test itself ever hanging a CI run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("refresh() threw under Node -- see internal/dashboard/js_smoke_test.go's own doc comment for why this matters:\n%s", out)
	}
	if !strings.Contains(string(out), "ALL_SCENARIOS_OK") {
		t.Fatalf("expected ALL_SCENARIOS_OK in Node output, got:\n%s", out)
	}
}
