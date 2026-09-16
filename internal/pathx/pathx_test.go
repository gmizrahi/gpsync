package pathx

import "testing"

// On Windows and macOS the filesystem ignores case, so two spellings of one
// path must compare equal; on Linux they are different files. Getting this
// wrong on macOS made the missing-file sweep report present files as gone.
func TestKeyAndSame_FollowThePlatformsCaseRules(t *testing.T) {
	for _, tc := range []struct {
		name        string
		insensitive bool
		a, b        string
		wantSame    bool
	}{
		{"case-insensitive host, same file", true, "/Photos/IMG.JPG", "/photos/img.jpg", true},
		{"case-insensitive host, different files", true, "/Photos/a.jpg", "/Photos/b.jpg", false},
		{"case-sensitive host, different files", false, "/Photos/IMG.JPG", "/photos/img.jpg", false},
		{"case-sensitive host, same file", false, "/Photos/a.jpg", "/Photos/a.jpg", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := caseInsensitive
			caseInsensitive = tc.insensitive
			defer func() { caseInsensitive = restore }()

			if got := Same(tc.a, tc.b); got != tc.wantSame {
				t.Errorf("Same(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.wantSame)
			}
			// Key must agree with Same: it is what map lookups rely on.
			if got := Key(tc.a) == Key(tc.b); got != tc.wantSame {
				t.Errorf("Key(%q) == Key(%q) is %v, want %v", tc.a, tc.b, got, tc.wantSame)
			}
		})
	}
}
