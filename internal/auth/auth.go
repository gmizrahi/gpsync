// Package auth handles OAuth: installed-app flow, token storage/refresh,
// and import from an existing rclone Google Photos remote so setup can be
// skipped when the user already has a working dedicated OAuth client.
//
// Secrets live only in ~/.gpsync/client_secret.json and ~/.gpsync/token.json —
// never in config.toml, and never printed or logged.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"gopkg.in/ini.v1"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

var (
	ClientSecretPath = filepath.Join(statedb.StateDir, "client_secret.json")
	TokenPath        = filepath.Join(statedb.StateDir, "token.json")
)

// Scopes are the OAuth scopes gpsync asks Google for.
//
// appendonly: upload media — everything gpsync actually does (uploads,
// mediaItems.batchCreate). It survived the April 2025 scope cuts. The scope
// also covers album creation, which gpsync used until album support was
// removed outright: the API rejected media item ids it had itself minted
// seconds earlier (see migrateDropAlbumTables in internal/statedb).
//
// Deliberately NOT requested: photoslibrary.readonly.appcreateddata. It
// existed only for the removed reconcile-against-the-API feature (see
// .ai/CLAUDE.md's "No reconcile-against-the-API step" rule); gpsync never reads
// media items back from Google, so asking for read access on a fresh
// consent screen would be requesting more than the tool needs. Dropping it
// only affects NEW consent flows (`gpsync setup`/`gpsync import-rclone`) —
// already-issued tokens keep whatever scopes they were granted, so existing
// working installs are unaffected.
var Scopes = []string{
	"https://www.googleapis.com/auth/photoslibrary.appendonly",
}

// ReadScope is the one scope that would let gpsync read media items back
// (mediaItems.get) rather than only append. It is deliberately NOT in
// Scopes above -- see that comment for why -- so anything needing it must
// check HasReadScope first and say so, rather than firing requests that
// can only fail.
const ReadScope = "https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata"

// HasReadScope reports whether this build requests read access at all.
//
// It answers a question that turned out to matter: `gpsync verify` was
// built on mediaItems.get without checking, and with an append-only
// credential Google answers those with 404 -- not 403 -- so every single
// file came back looking like a MISSING BACKUP. Fifty false alarms about
// photos that were demonstrably still there.
//
// Note what this can and cannot tell you: it reflects what THIS BUILD
// asks for, not what the stored token was actually granted. A token
// imported from rclone may carry broader scopes than gpsync would request
// itself, and token.json doesn't record them -- so a false here means
// "almost certainly cannot read", not "definitely cannot".
func HasReadScope() bool {
	for _, s := range Scopes {
		if s == ReadScope {
			return true
		}
	}
	return false
}

type ClientSecret struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func HasCredentials() bool {
	_, err := os.Stat(ClientSecretPath)
	return err == nil
}

func HasToken() bool {
	_, err := os.Stat(TokenPath)
	return err == nil
}

// CurrentClientID returns the OAuth client_id currently configured, for
// display purposes only (`gpsync config`, import confirmations). A client_id
// is a public identifier, not a secret — this deliberately never returns
// ClientSecret or token contents.
func CurrentClientID() (string, bool) {
	cs, err := loadClientSecret()
	if err != nil {
		return "", false
	}
	return cs.ClientID, true
}

func loadClientSecret() (*ClientSecret, error) {
	data, err := os.ReadFile(ClientSecretPath)
	if err != nil {
		return nil, fmt.Errorf("no OAuth client configured — run `gpsync setup` or `gpsync import-rclone` first")
	}
	var cs ClientSecret
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("client_secret.json is not valid: %w", err)
	}
	return &cs, nil
}

func SaveClientSecret(cs ClientSecret) error {
	if err := os.MkdirAll(filepath.Dir(ClientSecretPath), 0o700); err != nil {
		return err
	}
	// #nosec G117 -- this struct IS the credential file; marshalling it to
	// ~/.gpsync/client_secret.json at 0600 is the whole point.
	data, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(ClientSecretPath, data, 0o600); err != nil {
		return err
	}
	// Windows ignores the mode above; keep SECURITY.md's owner-only promise
	// with an ACL there. No-op on POSIX, where 0600 is already the answer.
	return restrictToOwner(ClientSecretPath)
}

func loadToken() *oauth2.Token {
	data, err := os.ReadFile(TokenPath)
	if err != nil {
		return nil
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil
	}
	return &tok
}

// saveToken writes token.json atomically: serialize to a sibling ".tmp"
// file, then rename it over the real path. os.WriteFile alone opens the
// live token.json with O_TRUNC and streams into it, so any failure (or any
// second writer) mid-write leaves a truncated/interleaved file behind --
// and an unparseable token.json is indistinguishable from "never
// authorized", silently dropping the next run into the interactive browser
// consent flow. Rename is atomic on both POSIX and Windows for same-volume
// paths, which this always is (the tmp file is in the same directory).
func saveToken(tok *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(TokenPath), 0o700); err != nil {
		return err
	}
	// #nosec G117 -- the OAuth token is written to ~/.gpsync/token.json at
	// 0600 by design; nothing here is logged or transmitted.
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	tmpPath := TokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, TokenPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// Applied AFTER the rename: the ACL belongs on the file that survives,
	// and a rename does not carry the staging file's DACL to the target.
	return restrictToOwner(TokenPath)
}

// persistingTokenSource writes the token back to disk whenever the
// underlying source refreshes it, so the next run picks up the new
// access token without a fresh consent flow.
//
// mu is not optional: x/oauth2's Transport.RoundTrip calls Token() from
// every request goroutine with no serialization of its own, so under real
// upload concurrency (default 6) the ~hourly refresh has several goroutines
// arriving at the check-and-write below at once. Without the lock they race
// on `last` (a plain data race) and, worse, pile into saveToken
// concurrently. Combined with the atomic rename above, this makes the
// on-disk token.json impossible to corrupt from within one process.
type persistingTokenSource struct {
	mu    sync.Mutex
	inner oauth2.TokenSource
	last  string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		_ = saveToken(tok)
	}
	return tok, nil
}

// GetHTTPClient returns an HTTP client that auto-attaches and auto-refreshes
// the OAuth bearer token, running the interactive consent flow if no token
// exists yet.
// baseHTTPClient bounds connection setup and time-to-first-response-byte,
// so a network hiccup (or a stalled token refresh) fails fast instead of
// hanging forever with no feedback. It deliberately does NOT set an overall
// Client.Timeout -- that would also cap large video upload bodies, which
// legitimately need to keep transferring past ResponseHeaderTimeout once
// the server has started responding.
func baseHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
		},
	}
}

func GetHTTPClient(ctx context.Context) (*http.Client, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, baseHTTPClient())

	cs, err := loadClientSecret()
	if err != nil {
		return nil, err
	}

	tok := loadToken()
	cfg := &oauth2.Config{
		ClientID:     cs.ClientID,
		ClientSecret: cs.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       Scopes,
	}

	if tok == nil {
		tok, err = runConsentFlow(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tok); err != nil {
			return nil, err
		}
	}

	base := oauth2.ReuseTokenSource(tok, cfg.TokenSource(ctx, tok))
	ts := &persistingTokenSource{inner: base, last: tok.AccessToken}
	return oauth2.NewClient(ctx, ts), nil
}

func runConsentFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth state mismatch")
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			fmt.Fprintln(w, "Authorization denied. You can close this window.")
			errCh <- fmt.Errorf("authorization denied: %s", errMsg)
			return
		}
		code := r.URL.Query().Get("code")
		fmt.Fprintln(w, "Authorized. You can close this window and return to the terminal.")
		codeCh <- code
	})
	// Timeouts bound a stalled client on the loopback OAuth callback
	// listener; the exchange itself is a single short request.
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		// http.ErrServerClosed is the normal shutdown path below; anything
		// else means the consent flow will never receive its callback, so
		// say so rather than leaving the user staring at a browser tab.
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "OAuth callback server stopped: %v\n", err)
		}
	}()
	defer server.Close()

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	fmt.Println("Open this URL to authorize gpsync (attempting to open your browser automatically):")
	fmt.Println(authURL)
	OpenBrowser(authURL)

	select {
	case code := <-codeCh:
		return cfg.Exchange(ctx, code)
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ── rclone import ───────────────────────────────────────────────────────

func FindRcloneConf() (string, bool) {
	if v := os.Getenv("RCLONE_CONFIG"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v, true
		}
	}

	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "rclone", "rclone.conf"))
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		candidates = append(candidates, filepath.Join(appdata, "rclone", "rclone.conf"))
	}
	// WSL: Windows user profile mounted under /mnt/c
	if matches, err := filepath.Glob("/mnt/c/Users/*/AppData/Roaming/rclone/rclone.conf"); err == nil {
		candidates = append(candidates, matches...)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, true
		}
	}
	return "", false
}

func ListGooglePhotosRemotes(confPath string) ([]string, error) {
	cfg, err := ini.Load(confPath)
	if err != nil {
		return nil, err
	}
	var remotes []string
	for _, section := range cfg.Sections() {
		if section.Key("type").String() == "google photos" {
			remotes = append(remotes, section.Name())
		}
	}
	return remotes, nil
}

type rcloneToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Expiry       string `json:"expiry"`
}

// ImportRemote imports a Google Photos remote's OAuth client + token from
// rclone.conf. Returns a human-readable message. Never logs the actual
// secret values.
func ImportRemote(confPath, remoteName string) (string, error) {
	cfg, err := ini.Load(confPath)
	if err != nil {
		return "", err
	}
	if !cfg.HasSection(remoteName) {
		return "", fmt.Errorf("remote %q not found in %s", remoteName, confPath)
	}
	section := cfg.Section(remoteName)
	clientID := section.Key("client_id").String()
	clientSecret := section.Key("client_secret").String()
	tokenRaw := section.Key("token").String()

	if clientID == "" || clientSecret == "" {
		return "", fmt.Errorf(
			"this remote has no dedicated client_id/client_secret — it's using rclone's shared " +
				"built-in OAuth app, which pools quota with every other rclone user. Run `gpsync setup` " +
				"to create your own dedicated GCP project instead of importing this")
	}
	if tokenRaw == "" {
		return "", fmt.Errorf("no token found on this remote — re-authenticate it in rclone first")
	}

	var rt rcloneToken
	if err := json.Unmarshal([]byte(tokenRaw), &rt); err != nil {
		return "", fmt.Errorf("could not parse the token field on this remote: %w", err)
	}

	if err := SaveClientSecret(ClientSecret{ClientID: clientID, ClientSecret: clientSecret}); err != nil {
		return "", err
	}

	expiry := time.Now() // safe default: treat as already-expired so the first real use refreshes it
	if rt.Expiry != "" {
		if parsed, err := time.Parse(time.RFC3339, rt.Expiry); err == nil {
			expiry = parsed
		}
	}
	tok := &oauth2.Token{
		AccessToken:  rt.AccessToken,
		RefreshToken: rt.RefreshToken,
		TokenType:    rt.TokenType,
		Expiry:       expiry,
	}
	if err := saveToken(tok); err != nil {
		return "", err
	}

	return fmt.Sprintf("Imported dedicated OAuth client from rclone remote %q.", remoteName), nil
}
