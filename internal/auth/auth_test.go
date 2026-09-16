package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func writeRcloneConf(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func setStateDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	ClientSecretPath = filepath.Join(dir, "client_secret.json")
	TokenPath = filepath.Join(dir, "token.json")
}

func TestListGooglePhotosRemotes(t *testing.T) {
	conf := writeRcloneConf(t, `
[gphotos]
type = google photos
client_id = 123.apps.googleusercontent.com
client_secret = shh
token = {"access_token":"a","refresh_token":"r","token_type":"Bearer","expiry":"2026-08-29T02:40:19Z"}

[somedrive]
type = drive
client_id =
`)
	remotes, err := ListGooglePhotosRemotes(conf)
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 1 || remotes[0] != "gphotos" {
		t.Errorf("got %v, want [gphotos]", remotes)
	}
}

func TestImportRemote_DedicatedClientSucceeds(t *testing.T) {
	setStateDir(t)
	conf := writeRcloneConf(t, `
[gphotos]
type = google photos
client_id = 123.apps.googleusercontent.com
client_secret = supersecret
token = {"access_token":"a","refresh_token":"r","token_type":"Bearer","expiry":"2026-08-29T02:40:19Z"}
`)

	msg, err := ImportRemote(conf, "gphotos")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg == "" {
		t.Error("expected a non-empty success message")
	}

	if !HasCredentials() {
		t.Fatal("expected client_secret.json to exist after import")
	}
	data, err := os.ReadFile(ClientSecretPath)
	if err != nil {
		t.Fatal(err)
	}
	var cs ClientSecret
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatal(err)
	}
	if cs.ClientID != "123.apps.googleusercontent.com" || cs.ClientSecret != "supersecret" {
		t.Errorf("unexpected client secret contents: %+v", cs)
	}

	if _, err := os.Stat(TokenPath); err != nil {
		t.Errorf("expected token.json to exist: %v", err)
	}
}

func TestImportRemote_SharedClientRejected(t *testing.T) {
	setStateDir(t)
	conf := writeRcloneConf(t, `
[gphotos]
type = google photos
client_id =
client_secret =
token = {"access_token":"a","refresh_token":"r"}
`)

	_, err := ImportRemote(conf, "gphotos")
	if err == nil {
		t.Fatal("expected an error for a remote with no dedicated client_id")
	}
	if HasCredentials() {
		t.Error("must not write credentials when rejecting a shared-client remote")
	}
}

func TestImportRemote_MissingRemote(t *testing.T) {
	setStateDir(t)
	conf := writeRcloneConf(t, `[other]
type = drive
`)
	_, err := ImportRemote(conf, "gphotos")
	if err == nil {
		t.Fatal("expected an error for a nonexistent remote")
	}
}

// fixedTokenSource hands out a NEW access token on every call, the way
// oauth2's refreshing source does at the ~hourly expiry boundary.
type fixedTokenSource struct {
	mu sync.Mutex
	n  int
}

func (f *fixedTokenSource) Token() (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return &oauth2.Token{
		AccessToken:  fmt.Sprintf("access-%d", f.n),
		RefreshToken: "refresh-constant",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, nil
}

// TestPersistingTokenSource_ConcurrentRefresh_LeavesTokenFileValid proves
// the fix for a corrupted token.json. x/oauth2's Transport.RoundTrip calls
// Token() from every request goroutine with no serialization of its own, so
// with the default concurrency of 6 in-flight uploads the hourly refresh
// had several goroutines hitting the check-and-write at once: an unguarded
// read/write of `last` (a plain data race, caught by -race) plus concurrent
// os.WriteFile(O_TRUNC) calls onto the live token.json. A truncated or
// interleaved token.json doesn't fail loudly -- it parses as "no token",
// silently dropping the NEXT run into the interactive browser consent flow.
//
// The file must be valid JSON with a usable refresh token after the storm,
// and must equal exactly one of the tokens that were actually issued (never
// a blend of two).
func TestPersistingTokenSource_ConcurrentRefresh_LeavesTokenFileValid(t *testing.T) {
	setStateDir(t)

	p := &persistingTokenSource{inner: &fixedTokenSource{}}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := p.Token(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(TokenPath)
	if err != nil {
		t.Fatalf("token.json unreadable after concurrent refreshes: %v", err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		t.Fatalf("token.json is not valid JSON after concurrent refreshes (this is the corruption that silently forces a fresh consent flow): %v -- contents: %q", err, data)
	}
	if tok.RefreshToken != "refresh-constant" {
		t.Errorf("refresh token = %q, want %q -- the persisted token is not a whole, coherent token", tok.RefreshToken, "refresh-constant")
	}
	if !strings.HasPrefix(tok.AccessToken, "access-") {
		t.Errorf("access token = %q, want one of the issued access-N values", tok.AccessToken)
	}
}

// TestSaveToken_NeverLeavesATruncatedFile proves saveToken is atomic
// regardless of caller: it stages to a sibling ".tmp" and renames, so an
// existing token.json is replaced whole. Writing a small token over a large
// one used to be an O_TRUNC-then-stream sequence, which is observable
// half-done; here the observable end state is a complete file, and no
// ".tmp" litter is left behind.
func TestSaveToken_NeverLeavesATruncatedFile(t *testing.T) {
	setStateDir(t)

	big := &oauth2.Token{AccessToken: strings.Repeat("x", 4096), RefreshToken: "r1", TokenType: "Bearer"}
	if err := saveToken(big); err != nil {
		t.Fatal(err)
	}
	small := &oauth2.Token{AccessToken: "y", RefreshToken: "r2", TokenType: "Bearer"}
	if err := saveToken(small); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		t.Fatalf("token.json is not valid JSON after overwriting a larger token: %v", err)
	}
	if tok.AccessToken != "y" || tok.RefreshToken != "r2" {
		t.Errorf("token.json = %+v, want the second (smaller) token", tok)
	}
	if _, err := os.Stat(TokenPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the staging file %s.tmp must not survive a successful save (err=%v)", TokenPath, err)
	}
	info, err := os.Stat(TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token.json mode = %o, want 600 -- the atomic rename must not loosen the documented credential permissions", perm)
	}
}

// TestScopes_RequestsOnlyWhatGPBActuallyUses proves the unused
// photoslibrary.readonly.appcreateddata scope was dropped: it existed only
// for the removed reconcile-against-the-API feature, and nothing in the
// codebase reads media items back from Google. A consent screen must not
// ask for read access the tool never exercises.
func TestScopes_RequestsOnlyWhatGPBActuallyUses(t *testing.T) {
	if len(Scopes) != 1 || Scopes[0] != "https://www.googleapis.com/auth/photoslibrary.appendonly" {
		t.Errorf("Scopes = %v, want exactly [photoslibrary.appendonly]", Scopes)
	}
	for _, s := range Scopes {
		if strings.Contains(s, "readonly") {
			t.Errorf("scope %q requests read access, but gpsync only ever writes (upload, batchCreate, albums.*)", s)
		}
	}
}
