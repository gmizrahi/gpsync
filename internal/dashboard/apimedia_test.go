package dashboard

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// addMediaFile writes a file and records it as a scan does: files_seen with
// its size and modification time, and a pending upload with its capture
// time.
func addMediaFile(t *testing.T, db *statedb.DB, sha, path string, data []byte, captured time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileSeen(path, sha, float64(info.ModTime().UnixNano())/1e9, info.Size()); err != nil {
		t.Fatal(err)
	}
	at := float64(captured.Unix())
	if err := db.EnsurePending(sha, info.Size(), "", path, &at); err != nil {
		t.Fatal(err)
	}
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func apiGet(t *testing.T, c *http.Client, u, token string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

type mediaFixture struct {
	db           *statedb.DB
	srv          string
	c            *http.Client
	tok          string
	photo, video string // hashes
	photoBytes   []byte
	photoPath    string
}

func newMediaFixture(t *testing.T) mediaFixture {
	t.Helper()
	db := openTestDB(t)
	dir := t.TempDir()
	f := mediaFixture{db: db, c: newTestClient(t), photo: fmt.Sprintf("%064x", 1), video: fmt.Sprintf("%064x", 2)}
	f.srv = newTestServer(t, db, newFakeController(), Options{}).URL
	f.tok = newToken(t, db, "viewer")
	f.photoBytes = testJPEG(t, 2000, 1000)
	f.photoPath = filepath.Join(dir, "p.jpg")
	addMediaFile(t, db, f.photo, f.photoPath, f.photoBytes, time.Date(2024, 5, 1, 9, 0, 0, 0, time.Local))
	addMediaFile(t, db, f.video, filepath.Join(dir, "v.mp4"), []byte("pretend video bytes"), time.Date(2024, 5, 1, 10, 0, 0, 0, time.Local))
	return f
}

// Issue #66's list over HTTP: its JSON shape, a page and its cursor, and a
// 400 for every parameter it can't use.
func TestAPIv1Media_List(t *testing.T) {
	f := newMediaFixture(t)
	resp, body := apiGet(t, f.c, f.srv+"/api/v1/media?limit=1", f.tok)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("list: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Next  *string          `json:"next"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 || page.Next == nil {
		t.Fatalf("first page = %s (%v); want one item and a next cursor", body, err)
	}
	want := map[string]any{"sha256": f.photo, "captured_at": time.Date(2024, 5, 1, 9, 0, 0, 0, time.Local).Format(time.RFC3339),
		"kind": "photo", "mime_type": "", "has_thumb": true,
		// #85: the paths, as stored. Every other field is as before.
		"first_path": f.photoPath, "paths": []any{f.photoPath}}
	if !reflect.DeepEqual(page.Items[0], want) {
		t.Errorf("item = %v\nwant   %v", page.Items[0], want)
	}
	_, body = apiGet(t, f.c, f.srv+"/api/v1/media?limit=1&cursor="+url.QueryEscape(*page.Next), f.tok)
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 || page.Items[0]["sha256"] != f.video || page.Next != nil {
		t.Errorf("second page = %s; want the video and next: null", body)
	}

	from := url.QueryEscape(time.Date(2024, 5, 1, 9, 30, 0, 0, time.Local).Format(time.RFC3339))
	_, body = apiGet(t, f.c, f.srv+"/api/v1/media?from="+from, f.tok)
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 || page.Items[0]["sha256"] != f.video {
		t.Errorf("from 09:30 = %s; want only the video", body)
	}
	_, body = apiGet(t, f.c, f.srv+"/api/v1/media?to=2000-01-01T00:00:00Z", f.tok)
	if strings.TrimSpace(string(body)) != `{"items":[],"next":null}` {
		t.Errorf("empty range = %s; want an empty items array, not null", body)
	}

	for _, q := range []string{"from=yesterday", "to=2024-05-01", "limit=0", "limit=1001", "limit=x", "cursor=bogus"} {
		if resp, body := apiGet(t, f.c, f.srv+"/api/v1/media?"+q, f.tok); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: %d %s, want 400", q, resp.StatusCode, body)
		}
	}
}

// A thumbnail: a JPEG at the snapped size, cached forever by content hash,
// 304 on a matching ETag; 415 for a video, 404 for an unknown hash or a
// missing original, 400 for something that isn't a hash.
func TestAPIv1Media_Thumbnail(t *testing.T) {
	f := newMediaFixture(t)
	u := f.srv + "/api/v1/media/" + f.photo + "/thumb?size=300"
	resp, body := apiGet(t, f.c, u, f.tok)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	img, err := jpeg.DecodeConfig(bytes.NewReader(body))
	if err != nil || img.Width != 512 || img.Height != 256 {
		t.Errorf("size=300 gave %dx%d (%v); want it snapped to 512 on the long edge", img.Width, img.Height, err)
	}
	etag := resp.Header.Get("ETag")
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || etag != `"`+f.photo+`-512"` {
		t.Errorf("Cache-Control %q, ETag %q; want immutable and the hash and size", cc, etag)
	}
	if resp, _ := apiGet(t, f.c, u, f.tok, "If-None-Match", etag); resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d, want 304", resp.StatusCode)
	}

	for what, c := range map[string]struct {
		path string
		want int
	}{
		"video":     {f.video + "/thumb", http.StatusUnsupportedMediaType},
		"unknown":   {fmt.Sprintf("%064x", 9) + "/thumb", http.StatusNotFound},
		"not a sha": {"ABC/thumb", http.StatusBadRequest},
		"bad size":  {f.photo + "/thumb?size=-1", http.StatusBadRequest},
	} {
		if resp, body := apiGet(t, f.c, f.srv+"/api/v1/media/"+c.path, f.tok); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", what, resp.StatusCode, body, c.want)
		}
	}

	// The original gone: a size not yet cached is a 404, not an error.
	if err := os.Remove(f.photoPath); err != nil {
		t.Fatal(err)
	}
	if resp, body := apiGet(t, f.c, f.srv+"/api/v1/media/"+f.photo+"/thumb?size=1024", f.tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing original: %d %s, want 404", resp.StatusCode, body)
	}
}

// The original: its exact bytes, with nosniff and ranges.
func TestAPIv1Media_Original(t *testing.T) {
	f := newMediaFixture(t)
	resp, body := apiGet(t, f.c, f.srv+"/api/v1/media/"+f.photo, f.tok)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, f.photoBytes) {
		t.Fatalf("original: %d, %d bytes; want 200 and the file's %d bytes", resp.StatusCode, len(body), len(f.photoBytes))
	}
	if resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("Content-Type %q, X-Content-Type-Options %q", resp.Header.Get("Content-Type"), resp.Header.Get("X-Content-Type-Options"))
	}
	resp, body = apiGet(t, f.c, f.srv+"/api/v1/media/"+f.video, f.tok, "Range", "bytes=0-6")
	if resp.StatusCode != http.StatusPartialContent || string(body) != "pretend" {
		t.Errorf("range: %d %q; want 206 and the first 7 bytes", resp.StatusCode, body)
	}
	for _, p := range []string{f.photo + "/", f.photo + "/thumb/x", f.photo + "/other"} {
		if resp, _ := apiGet(t, f.c, f.srv+"/api/v1/media/"+p, f.tok); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
}

// Media needs the read scope, and nothing under /api/v1/media writes to the
// ledger.
func TestAPIv1Media_ReadScopeAndReadOnly(t *testing.T) {
	f := newMediaFixture(t)
	control := newToken(t, f.db, "ctl", engine.ScopeControl)
	for _, p := range []string{"", "/" + f.photo, "/" + f.photo + "/thumb"} {
		if resp, _ := apiGet(t, f.c, f.srv+"/api/v1/media"+p, control); resp.StatusCode != http.StatusForbidden {
			t.Errorf("control-only token on media%s: %d, want 403", p, resp.StatusCode)
		}
	}

	snapshot := func() ([]statedb.Upload, []statedb.FileSeen) {
		ups, err := f.db.ListAll()
		if err != nil {
			t.Fatal(err)
		}
		var seen []statedb.FileSeen
		for _, u := range ups {
			s, err := f.db.FilesSeenWithSHA(u.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, s...)
		}
		return ups, seen
	}
	ups, seen := snapshot()
	for _, p := range []string{"", "?limit=1", "/" + f.photo, "/" + f.photo + "/thumb", "/" + f.video + "/thumb", "/" + fmt.Sprintf("%064x", 7)} {
		apiGet(t, f.c, f.srv+"/api/v1/media"+p, f.tok)
	}
	if ups2, seen2 := snapshot(); !reflect.DeepEqual(ups, ups2) || !reflect.DeepEqual(seen, seen2) {
		t.Error("a media request changed the ledger")
	}
}

// Thumbnails go to the thumbnail_dir setting; an invalid one -- here, the
// source folder itself -- is refused, and nothing is written anywhere.
func TestAPIv1Media_ThumbnailsGoToTheConfiguredFolder(t *testing.T) {
	db := openTestDB(t)
	lib := t.TempDir()
	sha := fmt.Sprintf("%064x", 1)
	addMediaFile(t, db, sha, filepath.Join(lib, "p.jpg"), testJPEG(t, 600, 400), time.Now())
	tok := newToken(t, db, "viewer")

	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{lib}
	ctrl.cfg.ThumbnailDir = filepath.Join(lib, ".thumbnails")
	srv := newTestServer(t, db, ctrl, Options{})
	if resp, body := apiGet(t, newTestClient(t), srv.URL+"/api/v1/media/"+sha+"/thumb", tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(lib, ".thumbnails", "256", sha+".jpg")); err != nil {
		t.Errorf("not cached in the configured folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(statedb.StateDir, "thumbs")); err == nil {
		t.Error("also cached in the default folder")
	}

	bad := newFakeController()
	bad.cfg.SourceFolders = []string{lib}
	bad.cfg.ThumbnailDir = lib
	srv = newTestServer(t, db, bad, Options{})
	if resp, _ := apiGet(t, newTestClient(t), srv.URL+"/api/v1/media/"+sha+"/thumb?size=512", tok); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("invalid thumbnail folder: %d, want 500", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(lib, "512")); err == nil {
		t.Error("a thumbnail was written into the source folder")
	}
}

// Issue #68's fields: present when known, absent when not, and captured_at
// in the camera's own zone when it recorded one.
func TestAPIv1Media_MetadataFields(t *testing.T) {
	f := newMediaFixture(t)
	at := float64(time.Date(2024, 5, 1, 7, 30, 0, 0, time.UTC).Unix())
	lat, lon, w, h := 48.8584, 2.2945, 4000, 3000
	if _, err := f.db.RecordMediaMetadata(f.photo, statedb.MediaMetadata{CapturedAt: &at, Source: "exif", Offset: "+02:00", Lat: &lat, Lon: &lon, Width: &w, Height: &h}, 1); err != nil {
		t.Fatal(err)
	}
	_, body := apiGet(t, f.c, f.srv+"/api/v1/media", f.tok)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 2 {
		t.Fatalf("%s (%v)", body, err)
	}
	byHash := map[string]map[string]any{}
	for _, it := range page.Items {
		byHash[it["sha256"].(string)] = it
	}
	p := byHash[f.photo]
	if p["captured_at"] != "2024-05-01T09:30:00+02:00" || p["offset"] != "+02:00" || p["captured_from"] != "exif" ||
		p["lat"] != lat || p["lon"] != lon || p["width"] != float64(w) || p["height"] != float64(h) {
		t.Errorf("photo = %v", p)
	}
	for _, k := range []string{"captured_from", "offset", "lat", "lon", "width", "height"} {
		if _, ok := byHash[f.video][k]; ok {
			t.Errorf("video has %q, which nothing recorded", k)
		}
	}
}

// #85: each item's paths, exactly as stored and behind the token like
// everything else -- a file seen at two paths lists both, sorted; a first
// path that's gone stays as first_path but leaves paths; Windows and Unix
// paths come back untouched; an item with no known path has neither field.
func TestAPIv1Media_Paths(t *testing.T) {
	db := openTestDB(t)
	at := float64(time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC).Unix())
	sha := func(i int) string { return fmt.Sprintf("%064x", i) }
	seed := func(s, first string, paths ...string) {
		t.Helper()
		if err := db.EnsurePending(s, 1, "image/jpeg", first, &at); err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if err := db.UpsertFileSeen(p, s, 1, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(sha(1), `D:\Pictures\2024_05 Trip\IMG_1.jpg`, `D:\Pictures\2024_05 Trip\IMG_1.jpg`, `D:\Pictures\Backup\IMG_1.jpg`)
	seed(sha(2), `C:\Users\me\Downloads\IMG-20240501-WA0003.jpg`, `D:\Pictures\Received\IMG-20240501-WA0003.jpg`) // first path gone
	seed(sha(3), "/home/me/Pictures/b.jpg", "/home/me/Pictures/b.jpg")
	seed(sha(4), "") // no known path
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)

	if resp, body := apiGet(t, c, srv.URL+"/api/v1/media", ""); resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), "Pictures") {
		t.Fatalf("without a token: %d %s; want 401 and no paths", resp.StatusCode, body)
	}
	tok := newToken(t, db, "viewer")
	_, body := apiGet(t, c, srv.URL+"/api/v1/media", tok)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]any{}
	for _, it := range page.Items {
		by[it["sha256"].(string)] = it
	}
	paths := func(it map[string]any) string {
		var out []string
		ps, _ := it["paths"].([]any)
		for _, p := range ps {
			out = append(out, p.(string))
		}
		return strings.Join(out, " | ")
	}
	for s, want := range map[string][2]string{
		sha(1): {`D:\Pictures\2024_05 Trip\IMG_1.jpg`, `D:\Pictures\2024_05 Trip\IMG_1.jpg | D:\Pictures\Backup\IMG_1.jpg`},
		sha(2): {`C:\Users\me\Downloads\IMG-20240501-WA0003.jpg`, `D:\Pictures\Received\IMG-20240501-WA0003.jpg`},
		sha(3): {"/home/me/Pictures/b.jpg", "/home/me/Pictures/b.jpg"},
	} {
		if it := by[s]; it["first_path"] != want[0] || paths(it) != want[1] {
			t.Errorf("%s…: first_path %v, paths %q; want %q, %q", s[60:], it["first_path"], paths(it), want[0], want[1])
		}
	}
	if _, has := by[sha(4)]["first_path"]; has {
		t.Errorf("an item with no known path has first_path: %v", by[sha(4)])
	}
	if _, has := by[sha(4)]["paths"]; has {
		t.Errorf("an item with no known path has paths: %v", by[sha(4)])
	}
}

// A full page of 1000 items, each at two paths, comes back in one request
// with every path right -- the paths come from one indexed lookup per page
// (see TestPathsBySHA256), not one per item.
func TestAPIv1Media_PathsOnAFullPage(t *testing.T) {
	db := openTestDB(t)
	at := float64(time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC).Unix())
	// Seeded in one transaction: 3000 separate writes made this test take
	// seconds of setup for a request that takes milliseconds.
	raw, err := sql.Open("sqlite", statedb.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < engine.MediaListMaxLimit; i++ {
		s := fmt.Sprintf("%064x", i)
		a, b := fmt.Sprintf(`D:\P\a%04d.jpg`, i), fmt.Sprintf(`D:\P\b%04d.jpg`, i)
		if _, err := tx.Exec(`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path, captured_at) VALUES (?, 1, 'image/jpeg', 'pending', ?, ?)`, s, a, at); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{b, a} {
			if _, err := tx.Exec(`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES (?, ?, 1, 1, 1)`, p, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	srv := newTestServer(t, db, newFakeController(), Options{})
	start := time.Now()
	_, body := apiGet(t, newTestClient(t), srv.URL+"/api/v1/media?limit=1000", newToken(t, db, "viewer"))
	t.Logf("a 1000-item page took %v", time.Since(start).Round(time.Millisecond))
	var page struct {
		Items []struct {
			SHA256 string   `json:"sha256"`
			Paths  []string `json:"paths"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1000 {
		t.Fatalf("%d items (%v)", len(page.Items), err)
	}
	for _, it := range page.Items {
		var i int
		fmt.Sscanf(it.SHA256, "%x", &i)
		if want := fmt.Sprintf(`D:\P\a%04d.jpg|D:\P\b%04d.jpg`, i, i); strings.Join(it.Paths, "|") != want {
			t.Fatalf("item %d: paths %v, want %s", i, it.Paths, want)
		}
	}
}
