package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gmizrahi/gpsync/internal/auth"
)

const (
	// A flow nobody finishes must not hold the callback listener forever.
	consentTimeout = 5 * time.Minute
	// A client_secret.json is a few hundred bytes. The cap is what stops an
	// authenticated but hostile upload from exhausting memory.
	maxCredentialUpload = 64 << 10
)

// consentTracker holds the one in-progress consent flow. Created fresh per
// Handler call, never a package var -- same reason loginAttemptTracker is.
//
// One at a time on purpose: two concurrent flows would both write
// token.json, and the loser's callback would arrive on a closed port.
type consentTracker struct {
	mu     sync.Mutex
	url    string
	err    error
	active bool
	done   bool
	cancel func()
}

func newConsentTracker() *consentTracker { return &consentTracker{} }

func (t *consentTracker) start() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active {
		return t.url, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), consentTimeout)
	c, err := auth.StartConsent(ctx)
	if err != nil {
		cancel()
		return "", err
	}
	t.active, t.done, t.err, t.url = true, false, nil, c.URL
	t.cancel = func() { cancel(); c.Cancel() }

	go func() {
		result := <-c.Done
		cancel()
		c.Cancel()
		t.mu.Lock()
		t.active, t.done, t.err = false, true, result
		t.mu.Unlock()
	}()
	return c.URL, nil
}

func (t *consentTracker) snapshot() (active, done bool, url string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active, t.done, t.url, t.err
}

// isLoopbackRequest reports whether the request came from this machine.
//
// The consent callback redirects to 127.0.0.1, so a flow started from
// another device can never complete -- the redirect lands on THAT device's
// loopback, where nothing is listening. Refused with an explanation rather
// than started and left to die silently.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleSignInUpload stores an uploaded client_secret.json.
//
// The file is a secret: it is parsed and written through auth (owner-only),
// and never logged or echoed back -- ParseClientSecretJSON's errors are
// tested not to include it.
func handleSignInUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialUpload)
	if err := r.ParseMultipartForm(maxCredentialUpload); err != nil {
		redirectSignIn(w, r, "", "that file is too large to be a client_secret.json")
		return
	}
	file, _, err := r.FormFile("credentials")
	if err != nil {
		redirectSignIn(w, r, "", "choose a client_secret.json file to upload")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxCredentialUpload))
	if err != nil {
		redirectSignIn(w, r, "", "could not read that file")
		return
	}
	cs, err := auth.ParseClientSecretJSON(data)
	if err != nil {
		redirectSignIn(w, r, "", err.Error())
		return
	}
	if err := auth.SaveClientSecret(cs); err != nil {
		redirectSignIn(w, r, "", "could not save the credentials")
		return
	}
	redirectSignIn(w, r, "Credentials saved. Now sign in with Google.", "")
}

func handleSignInStart(w http.ResponseWriter, r *http.Request, consent *consentTracker) {
	if !isLoopbackRequest(r) {
		redirectSignIn(w, r, "", "signing in has to be started on the computer running gpsync: "+
			"Google sends the browser back to that machine, so it cannot complete from another device")
		return
	}
	if !auth.HasCredentials() {
		redirectSignIn(w, r, "", "upload a client_secret.json first")
		return
	}
	url, err := consent.start()
	if err != nil {
		redirectSignIn(w, r, "", err.Error())
		return
	}
	// Local by definition (guarded above), so opening it here is right.
	auth.OpenBrowser(url)
	redirectSignIn(w, r, "", "")
}

func handleSignInStatus(w http.ResponseWriter, _ *http.Request, consent *consentTracker) {
	active, done, url, err := consent.snapshot()
	resp := struct {
		Active   bool   `json:"active"`
		Done     bool   `json:"done"`
		SignedIn bool   `json:"signed_in"`
		URL      string `json:"url,omitempty"`
		Error    string `json:"error,omitempty"`
	}{Active: active, Done: done, SignedIn: auth.HasToken(), URL: url}
	if err != nil {
		resp.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
