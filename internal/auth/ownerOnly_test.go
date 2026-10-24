package auth

import (
	"os"
	"testing"

	"golang.org/x/oauth2"

	"github.com/gmizrahi/gpsync/internal/fsperm/fspermtest"
)

// Issue #60. os.WriteFile applies its mode only when it CREATES the file, so
// a staging file left behind -- by an older version, a crash, another tool --
// with a looser mode used to hand that mode to token.json through the
// rename. RestrictToOwner is a no-op off Windows, so nothing corrected it.
func TestSaveToken_StaleLooseStagingFile_DoesNotLeakItsMode(t *testing.T) {
	setStateDir(t)
	if err := os.WriteFile(TokenPath+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(TokenPath+".tmp", 0o644); err != nil { // past any umask
		t.Fatal(err)
	}
	if err := saveToken(&oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	fspermtest.AssertOwnerOnly(t, TokenPath)
	if _, err := os.Stat(TokenPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the staging file was left behind: %v", err)
	}
}

// Issue #60, a fourth site the issue did not list: SaveClientSecret wrote
// client_secret.json in place with os.WriteFile, so an existing file kept
// whatever looser mode it already had.
func TestSaveClientSecret_LooseExistingFile_EndsOwnerOnly(t *testing.T) {
	setStateDir(t)
	if err := os.WriteFile(ClientSecretPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ClientSecretPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveClientSecret(ClientSecret{ClientID: "id", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	fspermtest.AssertOwnerOnly(t, ClientSecretPath)
}
