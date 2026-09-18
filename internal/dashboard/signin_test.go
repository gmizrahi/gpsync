package dashboard

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/version"
)

// withScratchCredentials points auth's credential paths at a temp dir.
//
// Not optional: those paths are computed once at package-init time from the
// real ~/.gpsync, so without this an import test would overwrite the
// developer's own client_secret.json and token.json. Mirrors
// internal/engine's withFakeOAuthCredentials.
func withScratchCredentials(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origSecret, origToken := auth.ClientSecretPath, auth.TokenPath
	auth.ClientSecretPath = filepath.Join(dir, "client_secret.json")
	auth.TokenPath = filepath.Join(dir, "token.json")
	t.Cleanup(func() { auth.ClientSecretPath, auth.TokenPath = origSecret, origToken })
	return dir
}

// writeRcloneConf creates a config with three sections: a Google Photos
// remote with its own OAuth client, one using rclone's shared built-in app
// (no client_id), and a non-Photos remote that must never be offered.
//
// RCLONE_CONFIG takes priority in FindRcloneConf, which is what keeps these
// tests off whatever rclone config the machine really has.
func writeRcloneConf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rclone.conf")
	body := `[photos-own]
type = google photos
client_id = own-client.apps.googleusercontent.com
client_secret = own-secret
token = {"access_token":"at","refresh_token":"rt","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"}

[photos-shared]
type = google photos
token = {"access_token":"at2","refresh_token":"rt2","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"}

[somedrive]
type = drive
token = {"access_token":"at3"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCLONE_CONFIG", path)
	return path
}

func TestSignInPage_ListsOnlyGooglePhotosRemotes(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)
	writeRcloneConf(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).Get(srv.URL + "/signin")
	mustNoErr(t, err)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{"photos-own", "photos-shared"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not offer remote %q", want)
		}
	}
	if strings.Contains(body, "somedrive") {
		t.Error("a non-Photos remote was offered for import")
	}
}

func TestSignInImportRclone_ImportsCredentialsAndToken(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)
	writeRcloneConf(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).PostForm(srv.URL+"/signin/import-rclone", url.Values{"remote": {"photos-own"}})
	mustNoErr(t, err)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", resp.StatusCode, readBody(t, resp))
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "imported=") {
		t.Errorf("Location = %q, want an imported= confirmation", loc)
	}

	if !auth.HasCredentials() {
		t.Fatal("no client_secret.json was written")
	}
	if !auth.HasToken() {
		t.Fatal("no token.json was written")
	}
	id, ok := auth.CurrentClientID()
	if !ok || id != "own-client.apps.googleusercontent.com" {
		t.Errorf("client ID = %q (ok=%v), want the remote's own client", id, ok)
	}

	// The token must carry the refresh token across, or the first expiry
	// would send the user back to a consent flow the tray cannot run.
	raw, err := os.ReadFile(auth.TokenPath)
	mustNoErr(t, err)
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	mustNoErr(t, json.Unmarshal(raw, &tok))
	if tok.RefreshToken != "rt" {
		t.Errorf("refresh_token = %q, want it carried over from rclone", tok.RefreshToken)
	}
}

// The remote name is client-supplied. It is validated against the list the
// server itself derived, so a crafted name never reaches the parser -- the
// same discipline backupFileFromRequest and originalsPathIsKnown apply.
func TestSignInImportRclone_UnknownRemote_RejectedAndWritesNothing(t *testing.T) {
	db := openTestDB(t)
	scratch := withScratchCredentials(t)
	writeRcloneConf(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	for _, remote := range []string{"somedrive", "does-not-exist", "../../etc/passwd"} {
		resp, err := newTestClient(t).PostForm(srv.URL+"/signin/import-rclone", url.Values{"remote": {remote}})
		mustNoErr(t, err)
		loc := resp.Header.Get("Location")
		resp.Body.Close()

		// Assert the GUARD's message specifically. ImportRemote would
		// refuse all three of these on its own, so "some error happened"
		// would pass with the membership check deleted -- this is what
		// proves the request never reached the parser at all.
		decoded, derr := url.QueryUnescape(loc)
		mustNoErr(t, derr)
		if !strings.Contains(decoded, "not a Google Photos remote in this rclone configuration") {
			t.Errorf("remote %q: message = %q, want the membership guard's refusal", remote, decoded)
		}
		if auth.HasCredentials() || auth.HasToken() {
			t.Fatalf("remote %q: credentials were written for a remote that is not importable", remote)
		}
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Errorf("wrote %d file(s) while rejecting every request", len(entries))
	}
}

// ImportRemote refuses a remote on rclone's shared built-in OAuth app and
// explains that it pools quota with every other rclone user. That reason is
// the useful part, so it must reach the page rather than being flattened
// into "import failed".
func TestSignInImportRclone_SharedRcloneApp_SurfacesTheReason(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)
	writeRcloneConf(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).PostForm(srv.URL+"/signin/import-rclone", url.Values{"remote": {"photos-shared"}})
	mustNoErr(t, err)
	loc := resp.Header.Get("Location")
	resp.Body.Close()

	if !strings.Contains(loc, "error=") {
		t.Fatalf("Location = %q, want a refusal", loc)
	}
	decoded, err := url.QueryUnescape(loc)
	mustNoErr(t, err)
	if !strings.Contains(decoded, "shared") || !strings.Contains(decoded, "quota") {
		t.Errorf("message = %q, want ImportRemote's own explanation about the shared app and quota", decoded)
	}
	if auth.HasCredentials() {
		t.Error("credentials were written despite the refusal")
	}
}

// The nav link is only worth showing while there is nothing to sync with.
//
// Asserts against the NAV specifically, not the whole page: the Settings
// Account card also links to /signin with the same words while credentials
// are missing, so a body-wide search matches that instead and passes (or
// fails) for the wrong reason. A first version of this test did exactly
// that.
func TestSignInNavLink_OnlyWhileSignedOut(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	client := newTestClient(t)

	resp, err := client.Get(srv.URL + "/settings")
	mustNoErr(t, err)
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(navOf(t, body), `href="/signin"`) {
		t.Error("no Sign in link in the nav while signed out")
	}

	// Signed in means BOTH a client and a token -- a token alone leaves
	// HasCredentials false, which is the state the card hint renders for.
	mustNoErr(t, os.WriteFile(auth.ClientSecretPath,
		[]byte(`{"client_id":"c.apps.googleusercontent.com","client_secret":"s"}`), 0o600))
	mustNoErr(t, os.WriteFile(auth.TokenPath, []byte(`{"access_token":"x"}`), 0o600))

	resp, err = client.Get(srv.URL + "/settings")
	mustNoErr(t, err)
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(navOf(t, body), `href="/signin"`) {
		t.Error("Sign in link still in the nav after credentials and a token exist")
	}
}

// navOf returns just the nav region, so assertions cannot accidentally
// match a link elsewhere on the page -- the Settings Account card links to
// /signin with the same words.
//
// pageShell renders the links in <div class="nav">, not a <nav> element;
// scoping on the wrong tag made this fatal in every state, which made the
// test red regardless and its break-check meaningless.
func navOf(t *testing.T, body string) string {
	t.Helper()
	const open = `<div class="nav">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("page has no %s region", open)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		t.Fatal("nav region is unterminated")
	}
	return rest[:j]
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// The top bar and tab title carry the build version, so a dashboard left
// open in a tab says which build it is. Asserts on both places the app
// name is rendered, since they are interpolated separately.
func TestPageShell_TitleCarriesTheVersion(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).Get(srv.URL + "/settings")
	mustNoErr(t, err)
	body := readBody(t, resp)
	resp.Body.Close()

	want := "GPhotos Sync " + version.Version
	if !strings.Contains(body, "<h1>"+want+"</h1>") {
		t.Errorf("top bar does not read %q", want)
	}
	if !strings.Contains(body, "— "+want+"</title>") {
		t.Errorf("tab title does not end with %q", want)
	}
	// The version must be a real value, not an empty string quietly
	// rendering as a trailing space.
	if version.Version == "" {
		t.Fatal("version.Version is empty")
	}
}

func multipartCredentials(t *testing.T, body string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, err := w.CreateFormFile("credentials", "client_secret.json")
	mustNoErr(t, err)
	_, err = f.Write([]byte(body))
	mustNoErr(t, err)
	mustNoErr(t, w.Close())
	return w.FormDataContentType(), &buf
}

func TestSignInUpload_StoresAValidDesktopClient(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	ct, body := multipartCredentials(t, `{"installed":{"client_id":"abc.apps.googleusercontent.com","client_secret":"shh"}}`)
	resp, err := newTestClient(t).Post(srv.URL+"/signin/upload", ct, body)
	mustNoErr(t, err)
	resp.Body.Close()

	if !auth.HasCredentials() {
		t.Fatal("credentials were not stored")
	}
	id, _ := auth.CurrentClientID()
	if id != "abc.apps.googleusercontent.com" {
		t.Errorf("client ID = %q", id)
	}
}

// A Web client cannot use the loopback redirect. It must be refused, and
// the refusal must not leak the secret it contained.
func TestSignInUpload_RejectsWebClientWithoutLeakingTheSecret(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	const secret = "leaky-secret-value"
	ct, body := multipartCredentials(t, `{"web":{"client_id":"abc","client_secret":"`+secret+`"}}`)
	resp, err := newTestClient(t).Post(srv.URL+"/signin/upload", ct, body)
	mustNoErr(t, err)
	loc := resp.Header.Get("Location")
	resp.Body.Close()

	if auth.HasCredentials() {
		t.Fatal("a Web client was stored")
	}
	decoded, err := url.QueryUnescape(loc)
	mustNoErr(t, err)
	if !strings.Contains(decoded, "Web application") {
		t.Errorf("message = %q, want the Web client named", decoded)
	}
	if strings.Contains(decoded, secret) {
		t.Fatalf("the redirect leaked the client secret: %q", decoded)
	}
}

func TestSignInUpload_RejectsOversizedFile(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	ct, body := multipartCredentials(t, strings.Repeat("x", maxCredentialUpload+1024))
	resp, err := newTestClient(t).Post(srv.URL+"/signin/upload", ct, body)
	if err == nil {
		resp.Body.Close()
	}
	if auth.HasCredentials() {
		t.Fatal("an oversized upload was stored")
	}
}

// The consent callback redirects to 127.0.0.1 on this machine, so a flow
// started from elsewhere can never complete. It must be refused with a
// reason rather than started and left to die.
func TestSignInStart_RefusesNonLoopbackRequests(t *testing.T) {
	withScratchCredentials(t)
	mustNoErr(t, os.WriteFile(auth.ClientSecretPath,
		[]byte(`{"client_id":"c.apps.googleusercontent.com","client_secret":"s"}`), 0o600))

	consent := newConsentTracker()
	req := httptest.NewRequest(http.MethodPost, "/signin/start", nil)
	req.RemoteAddr = "192.0.2.10:54321" // documentation range: never loopback
	rec := httptest.NewRecorder()

	handleSignInStart(rec, req, consent)

	loc := rec.Header().Get("Location")
	decoded, err := url.QueryUnescape(loc)
	mustNoErr(t, err)
	if !strings.Contains(decoded, "computer running gpsync") {
		t.Errorf("message = %q, want it to explain where sign-in must happen", decoded)
	}
	if active, _, _, _ := consent.snapshot(); active {
		t.Error("a consent flow was started for a remote request")
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:1234", true},
		{"192.0.2.10:1234", false},
		{"10.0.0.5:80", false},
		{"garbage", false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/signin/start", nil)
		req.RemoteAddr = tc.addr
		if got := isLoopbackRequest(req); got != tc.want {
			t.Errorf("isLoopbackRequest(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestSignInStatus_ReportsIdleState(t *testing.T) {
	db := openTestDB(t)
	withScratchCredentials(t)

	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).Get(srv.URL + "/signin/status")
	mustNoErr(t, err)
	defer resp.Body.Close()

	var s struct {
		Active   bool `json:"active"`
		Done     bool `json:"done"`
		SignedIn bool `json:"signed_in"`
	}
	mustNoErr(t, json.NewDecoder(resp.Body).Decode(&s))
	if s.Active || s.Done || s.SignedIn {
		t.Errorf("idle status = %+v, want all false", s)
	}
}
