package engine

import (
	"path/filepath"
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

// Settings sets the thumbnail folder, and refuses a save that would make it
// hide a source folder -- whichever of the two fields the save changes.
func TestApplySettingsForm_ThumbnailFolder(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "Pictures")
	form := validSettingsForm()
	form.Set("source_folders", lib)
	form.Set("thumbnail_dir", filepath.Join(lib, ".thumbnails"))
	cfg, err := ApplySettingsForm(config.Defaults(), form)
	if err != nil || cfg.ThumbnailDir != filepath.Join(lib, ".thumbnails") {
		t.Fatalf("inside the library: %q, %v; want it saved", cfg.ThumbnailDir, err)
	}

	form.Set("thumbnail_dir", filepath.Dir(lib))
	if _, err := ApplySettingsForm(cfg, form); err == nil {
		t.Error("a thumbnail folder containing the library was saved")
	}
	form.Set("thumbnail_dir", cfg.ThumbnailDir)
	form.Set("source_folders", lib+"\n"+filepath.Join(cfg.ThumbnailDir, "more"))
	if _, err := ApplySettingsForm(cfg, form); err == nil {
		t.Error("a source folder inside the thumbnail folder was saved")
	}

	form = validSettingsForm()
	form.Set("source_folders", lib)
	if got, err := ApplySettingsForm(cfg, form); err != nil || got.ThumbnailDir != cfg.ThumbnailDir {
		t.Errorf("a form without the field: %q, %v; want the setting kept", got.ThumbnailDir, err)
	}
}
