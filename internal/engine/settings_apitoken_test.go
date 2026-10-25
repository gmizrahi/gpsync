package engine

import (
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
)

// The lifetime setting is applied only when the form sends it: a form
// without the field must leave it alone -- the TLS settings were once reset
// on every save exactly this way (issue #67).
func TestApplySettingsForm_APITokenLifetime(t *testing.T) {
	cur := config.Defaults()
	cur.APITokenLifetime = "30d"
	form := validSettingsForm()
	form.Del("api_token_lifetime")
	got, err := ApplySettingsForm(cur, form)
	if err != nil {
		t.Fatal(err)
	}
	if got.APITokenLifetime != "30d" {
		t.Errorf("a form without the field changed the lifetime to %q", got.APITokenLifetime)
	}
	form.Set("api_token_lifetime", " NEVER ")
	if got, err := ApplySettingsForm(cur, form); err != nil || got.APITokenLifetime != "never" {
		t.Errorf("lifetime = %q, %v; want never", got.APITokenLifetime, err)
	}
	form.Set("api_token_lifetime", "soon")
	if _, err := ApplySettingsForm(cur, form); err == nil {
		t.Error("an invalid lifetime was accepted")
	}
}
