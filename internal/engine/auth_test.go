package engine

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestGenerateSessionToken_ReturnsUniqueNonEmptyHexTokens(t *testing.T) {
	a, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatal("token is empty")
	}
	if a == b {
		t.Fatal("two calls returned the same token -- not random")
	}
	if len(a) != 64 { // 32 bytes, hex-encoded
		t.Errorf("len(token) = %d, want 64", len(a))
	}
}

func TestCredentialsValid(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, user, pass string
		want             bool
	}{
		{"correct user and pass", "alice", "correct-horse", true},
		{"wrong password", "alice", "wrong", false},
		{"wrong username", "bob", "correct-horse", false},
		{"empty password", "alice", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CredentialsValid(c.user, c.pass, "alice", string(hash)); got != c.want {
				t.Errorf("CredentialsValid(%q, %q) = %v, want %v", c.user, c.pass, got, c.want)
			}
		})
	}
}

func TestLoginRedirectTarget(t *testing.T) {
	cases := []struct {
		next, want string
	}{
		{"", "/"},
		{"/settings", "/settings"},
		{"/browse?type=pending", "/browse?type=pending"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example", "/"},
		{"http://evil.example/", "/"},
		{"evil.example", "/"},
		// A verified bypass of the two checks above: browsers strip
		// ASCII tab/newline/CR from a URL before parsing it, so a tab at
		// index 1 sails past the next[1]=='/' check, and after stripping
		// resolves to protocol-relative "//evil.example".
		{"/\t/evil.example", "/"},
		{"/\t\\evil.example", "/"},
		{"/settings\t", "/"},
		{"/\n/evil.example", "/"},
		{"/\r/evil.example", "/"},
	}
	for _, c := range cases {
		t.Run(c.next, func(t *testing.T) {
			if got := LoginRedirectTarget(c.next); got != c.want {
				t.Errorf("LoginRedirectTarget(%q) = %q, want %q", c.next, got, c.want)
			}
		})
	}
}
