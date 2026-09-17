package dashboard

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testHosts() []string { return []string{"localhost", "127.0.0.1", "192.168.1.50"} }

// A generated pair must actually be usable: the cert and key must load
// together, cover every host asked for, and the key must not be readable by
// anyone else. The permission check is the one that matters most -- this
// file is a private key sitting next to the OAuth token.
func TestEnsureTLSFiles_GeneratesAUsablePair(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	got, err := EnsureTLSFiles(dir, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("EnsureTLSFiles: %v", err)
	}
	if !got.Generated {
		t.Error("Generated = false, want true on a first call into an empty directory")
	}
	if got.Supplied {
		t.Error("Supplied = true, want false for a pair gpsync manages itself")
	}
	if got.CertPath != filepath.Join(dir, TLSCertFileName) || got.KeyPath != filepath.Join(dir, TLSKeyFileName) {
		t.Errorf("paths = %q/%q, want them under %q", got.CertPath, got.KeyPath, dir)
	}

	leaf, err := loadLeaf(got.CertPath, got.KeyPath)
	if err != nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}
	for _, h := range testHosts() {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("certificate does not cover %q: %v", h, err)
		}
	}
	if leaf.NotAfter.Before(now.Add(800 * 24 * time.Hour)) {
		t.Errorf("NotAfter = %v, want roughly 825 days out so it is not re-trusted yearly", leaf.NotAfter)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(got.KeyPath)
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("key mode = %o, want 600 -- a private key must not be group/world readable", perm)
		}
	}

	// No staging file may survive, or `gpsync backup` would archive it.
	if _, err := os.Stat(got.KeyPath + ".tmp"); !os.IsNotExist(err) {
		t.Error("a .tmp staging file was left behind next to the key")
	}
}

// The whole point of this being safe to call on every start: a second call
// must reuse what is there rather than writing a new key, which would
// change the fingerprint and invalidate the trust the user established.
func TestEnsureTLSFiles_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	first, err := EnsureTLSFiles(dir, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("first EnsureTLSFiles: %v", err)
	}
	second, err := EnsureTLSFiles(dir, "", "", testHosts(), now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("second EnsureTLSFiles: %v", err)
	}

	if second.Generated {
		t.Error("Generated = true on the second call; an existing usable pair must be reused")
	}
	if second.Fingerprint != first.Fingerprint {
		t.Errorf("fingerprint changed across calls (%s -> %s); the user would have to re-trust it",
			first.Fingerprint, second.Fingerprint)
	}
}

// A machine's LAN address changes. A certificate that no longer covers the
// address the dashboard is reached on is useless, so it must be replaced
// rather than served with a name mismatch.
func TestEnsureTLSFiles_RegeneratesWhenANewHostIsNeeded(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	first, err := EnsureTLSFiles(dir, "", "", []string{"localhost", "127.0.0.1"}, now)
	if err != nil {
		t.Fatalf("first EnsureTLSFiles: %v", err)
	}
	second, err := EnsureTLSFiles(dir, "", "", []string{"localhost", "127.0.0.1", "10.0.0.7"}, now)
	if err != nil {
		t.Fatalf("second EnsureTLSFiles: %v", err)
	}

	if !second.Generated {
		t.Fatal("Generated = false, want a new pair once a host is no longer covered")
	}
	if second.Fingerprint == first.Fingerprint {
		t.Error("fingerprint unchanged, so the certificate was not actually replaced")
	}
	leaf, err := loadLeaf(second.CertPath, second.KeyPath)
	if err != nil {
		t.Fatalf("regenerated pair does not load: %v", err)
	}
	if err := leaf.VerifyHostname("10.0.0.7"); err != nil {
		t.Errorf("regenerated certificate still does not cover the new address: %v", err)
	}
}

// Renewal happens before expiry, not at it -- otherwise the first start
// after the certificate lapses is a startup failure.
func TestEnsureTLSFiles_RenewsBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	first, err := EnsureTLSFiles(dir, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("first EnsureTLSFiles: %v", err)
	}

	// Inside the renewal window but still genuinely valid.
	soon := first.NotAfter.Add(-tlsRenewBefore).Add(time.Hour)
	second, err := EnsureTLSFiles(dir, "", "", testHosts(), soon)
	if err != nil {
		t.Fatalf("second EnsureTLSFiles: %v", err)
	}
	if !second.Generated {
		t.Error("Generated = false inside the renewal window; it would expire mid-use")
	}
}

// A truncated or half-written pair must be replaced, not turned into a
// startup error the user can only fix by deleting files by hand.
func TestEnsureTLSFiles_ReplacesACorruptPair(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if _, err := EnsureTLSFiles(dir, "", "", testHosts(), now); err != nil {
		t.Fatalf("first EnsureTLSFiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, TLSCertFileName), []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupting the certificate: %v", err)
	}

	got, err := EnsureTLSFiles(dir, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("EnsureTLSFiles over a corrupt pair: %v", err)
	}
	if !got.Generated {
		t.Error("Generated = false, want the unreadable pair replaced")
	}
	if _, err := loadLeaf(got.CertPath, got.KeyPath); err != nil {
		t.Errorf("pair still does not load after regeneration: %v", err)
	}
}

// A user-supplied certificate is theirs. gpsync must serve it and never
// write over it, since regenerating would destroy a key it did not create.
func TestEnsureTLSFiles_SuppliedPairIsUsedAndNeverRewritten(t *testing.T) {
	supplied := t.TempDir()
	managed := t.TempDir()
	now := time.Now()

	seed, err := EnsureTLSFiles(supplied, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("seeding a stand-in user certificate: %v", err)
	}
	before, err := os.ReadFile(seed.KeyPath)
	if err != nil {
		t.Fatalf("reading seeded key: %v", err)
	}

	got, err := EnsureTLSFiles(managed, seed.CertPath, seed.KeyPath, testHosts(), now)
	if err != nil {
		t.Fatalf("EnsureTLSFiles with a supplied pair: %v", err)
	}
	if !got.Supplied || got.Generated {
		t.Errorf("Supplied=%v Generated=%v, want true/false for a configured pair", got.Supplied, got.Generated)
	}
	if got.CertPath != seed.CertPath || got.KeyPath != seed.KeyPath {
		t.Errorf("paths = %q/%q, want the configured ones", got.CertPath, got.KeyPath)
	}
	if got.Fingerprint != seed.Fingerprint {
		t.Errorf("fingerprint = %s, want the supplied certificate's %s", got.Fingerprint, seed.Fingerprint)
	}

	after, err := os.ReadFile(seed.KeyPath)
	if err != nil {
		t.Fatalf("re-reading seeded key: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the supplied key was rewritten; a user-managed key must never be touched")
	}
	if entries, err := os.ReadDir(managed); err == nil && len(entries) != 0 {
		t.Errorf("wrote %d file(s) into the managed directory despite a supplied pair", len(entries))
	}
}

// Half a pair is a configuration mistake. Generating the missing half would
// pair a real certificate with an invented key -- every handshake would
// fail, looking like file corruption rather than the mistake it is.
func TestEnsureTLSFiles_HalfConfiguredPairIsRejected(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, cert, key string }{
		{"cert without key", "/tmp/c.pem", ""},
		{"key without cert", "", "/tmp/k.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EnsureTLSFiles(dir, tc.cert, tc.key, testHosts(), time.Now()); err == nil {
				t.Fatal("err = nil, want a refusal naming the incomplete pair")
			}
		})
	}
}

// A missing configured file must say so plainly, and must NOT silently fall
// back to generating one -- that would serve a certificate the user never
// asked for while their own sat unused behind a typo.
func TestEnsureTLSFiles_MissingSuppliedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	_, err := EnsureTLSFiles(dir, filepath.Join(dir, "absent.pem"), filepath.Join(dir, "absent.key"), testHosts(), time.Now())
	if err == nil {
		t.Fatal("err = nil, want an error for a configured file that does not exist")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("generated %d file(s) as a fallback; a configured path must never be silently replaced", len(entries))
	}
}

// The fingerprint is what the user compares against the browser's warning,
// so it has to be in the form browsers actually print.
func TestFingerprint_MatchesTheFormBrowsersShow(t *testing.T) {
	dir := t.TempDir()
	got, err := EnsureTLSFiles(dir, "", "", testHosts(), time.Now())
	if err != nil {
		t.Fatalf("EnsureTLSFiles: %v", err)
	}

	parts := strings.Split(got.Fingerprint, ":")
	if len(parts) != sha256Bytes {
		t.Fatalf("fingerprint has %d groups, want %d", len(parts), sha256Bytes)
	}
	for _, p := range parts {
		if len(p) != 2 || strings.ToUpper(p) != p {
			t.Fatalf("group %q is not a two-digit uppercase hex byte", p)
		}
	}

	leaf, err := loadLeaf(got.CertPath, got.KeyPath)
	if err != nil {
		t.Fatalf("loadLeaf: %v", err)
	}
	if Fingerprint(leaf) != got.Fingerprint {
		t.Error("Fingerprint(leaf) disagrees with the reported fingerprint")
	}
}

const sha256Bytes = 32

// Loopback must always be covered: it is the default bind, and a dashboard
// that cannot be reached at 127.0.0.1 over TLS is broken for most installs.
func TestDefaultCertHosts_AlwaysCoversLoopback(t *testing.T) {
	hosts := DefaultCertHosts()
	for _, want := range []string{"localhost", "127.0.0.1", "::1"} {
		found := false
		for _, h := range hosts {
			if h == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultCertHosts() = %v, missing %q", hosts, want)
		}
	}

	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h] {
			t.Errorf("duplicate host %q; duplicates become duplicate SANs", h)
		}
		seen[h] = true
	}
}

// Guards the claim in EnsureTLSFiles' doc comment that a generated
// certificate can serve as its own trust anchor -- Windows will not accept
// a non-CA certificate in the root store, so trusting it would be
// impossible and the warning could never be cleared.
func TestGeneratedCertificate_CanBeATrustAnchor(t *testing.T) {
	dir := t.TempDir()
	got, err := EnsureTLSFiles(dir, "", "", testHosts(), time.Now())
	if err != nil {
		t.Fatalf("EnsureTLSFiles: %v", err)
	}
	leaf, err := loadLeaf(got.CertPath, got.KeyPath)
	if err != nil {
		t.Fatalf("loadLeaf: %v", err)
	}
	if !leaf.IsCA || !leaf.BasicConstraintsValid {
		t.Errorf("IsCA=%v BasicConstraintsValid=%v, want both true so it can be trusted as a root",
			leaf.IsCA, leaf.BasicConstraintsValid)
	}

	// And it must verify against itself, which is what a browser does once
	// the user has added it to their trust store.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "localhost",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("certificate does not verify against itself as a root: %v", err)
	}
}
