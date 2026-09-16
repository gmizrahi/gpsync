package engine

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// GenerateSessionToken returns a random 256-bit hex-encoded token for a
// dashboard login session -- unguessable by construction, so unlike a
// password compare, looking one up by exact match (a plain SQL WHERE) is
// fine: there's no secret-dependent timing side channel worth defending
// when the token itself is 32 bytes of crypto/rand.
func GenerateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CredentialsValid checks a submitted username/password against the
// configured dashboard credentials -- the same constant-time username
// compare + bcrypt password compare the old HTTP Basic Auth middleware did
// on every single request, now done once at login time instead (a session
// token check on every subsequent request is a plain, cheap lookup, not a
// bcrypt hash on every page load).
func CredentialsValid(user, pass, wantUser, wantPassHash string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(wantPassHash), []byte(pass)) == nil
	return userOK && passOK
}

// LoginRedirectTarget sanitizes a `next` query/form value into a safe,
// same-site relative path to send a browser to after a successful login --
// guarding against an open redirect. A `next` that doesn't start with
// exactly one leading "/" is rejected outright (that covers an absolute
// URL like "https://evil.example", which doesn't start with "/" at all);
// "//evil.example" is ALSO rejected even though it starts with "/", since
// browsers treat a leading "//" as protocol-relative and will still
// navigate off-site -- same for "/\evil.example", a known bypass for
// filters that only check for "//" (some browsers normalize a leading
// backslash to a forward slash before resolving the URL).
//
// Also rejects any ASCII control byte (0x00-0x1F, 0x7F) anywhere in the
// string, which is a verified bypass of the two checks above: the WHATWG
// URL spec has browsers strip ASCII tab/newline/CR from a URL before
// parsing it, so "/\t/evil.example" starts with "/" and has '/' at index
// 2, not index 1, sailing past both checks -- then a browser strips the
// tab, resolves "//evil.example" as protocol-relative, and navigates off
// the site the user just authenticated to. '\n'/'\r' get neutralized by
// Go's own header writer (turned into spaces) but tab does not, and
// nothing guarantees every future caller of this function writes the
// result through an http.Header the same way, so this is rejected at the
// source rather than relied on downstream.
func LoginRedirectTarget(next string) string {
	if next == "" || next[0] != '/' {
		return "/"
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return "/"
	}
	for i := 0; i < len(next); i++ {
		if next[i] < 0x20 || next[i] == 0x7f {
			return "/"
		}
	}
	return next
}
