package auth

import (
	"strings"
	"testing"
)

func TestParseClientSecretJSON_DesktopClient(t *testing.T) {
	data := []byte(`{"installed":{"client_id":"abc.apps.googleusercontent.com","client_secret":"shh","redirect_uris":["http://localhost"]}}`)

	cs, err := ParseClientSecretJSON(data)
	if err != nil {
		t.Fatalf("ParseClientSecretJSON: %v", err)
	}
	if cs.ClientID != "abc.apps.googleusercontent.com" {
		t.Errorf("ClientID = %q", cs.ClientID)
	}
	if cs.ClientSecret != "shh" {
		t.Errorf("ClientSecret not carried across")
	}
}

// Google issues Web clients from the same console page, and picking the
// wrong type is an easy mistake. A Web client cannot use the loopback
// redirect the consent flow depends on, so it must be named rather than
// stored and left to fail later at an unrelated point.
func TestParseClientSecretJSON_WebClient_IsNamedNotAccepted(t *testing.T) {
	data := []byte(`{"web":{"client_id":"abc.apps.googleusercontent.com","client_secret":"shh"}}`)

	_, err := ParseClientSecretJSON(data)
	if err == nil {
		t.Fatal("err = nil, want a Web client refused")
	}
	// "Web application", not "Desktop": BOTH this message and the generic
	// fallback mention Desktop, so asserting on that word passes even with
	// the Web-specific branch deleted -- the test then proves nothing.
	if got := err.Error(); !strings.Contains(got, "Web application") {
		t.Errorf("error = %q, want it to name the Web client specifically", got)
	}
}

func TestParseClientSecretJSON_Rejects(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", `not json at all`},
		{"empty object", `{}`},
		{"installed without client_id", `{"installed":{"client_secret":"shh"}}`},
		{"empty file", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseClientSecretJSON([]byte(tc.body)); err == nil {
				t.Fatal("err = nil, want a refusal")
			}
		})
	}
}

// A secret must never be echoed back in an error -- the message can reach a
// browser page or a log.
func TestParseClientSecretJSON_ErrorNeverLeaksTheSecret(t *testing.T) {
	const secret = "super-secret-value"
	data := []byte(`{"web":{"client_id":"abc","client_secret":"` + secret + `"}}`)

	_, err := ParseClientSecretJSON(data)
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message contains the client secret: %q", err.Error())
	}
}
