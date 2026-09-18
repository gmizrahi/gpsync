package auth

import "testing"

// OpenBrowser must never launch anything from a test: it puts a window on a
// real desktop, and an OAuth consent screen at that. If this ever fails,
// running the suite can hijack someone's browser.
func TestOpenBrowser_IsANoOpUnderTest(t *testing.T) {
	if !testing.Testing() {
		t.Fatal("testing.Testing() is false inside a test")
	}
	// Returns immediately rather than spawning anything.
	OpenBrowser("https://example.invalid/should-never-open")
}
