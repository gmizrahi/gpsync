package pathx

import (
	"path/filepath"
	"testing"
)

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

// Within decides whether a thumbnail folder would hide a source folder from
// scans, so a near-miss name ("Pictures2" beside "Pictures") must not
// count, and on a case-insensitive host neither must the spelling.
func TestWithin(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "Pictures"
	for _, tc := range []struct {
		insensitive bool
		p           string
		want        bool
	}{
		{false, root, true},
		{false, root + sep + "Trip" + sep + "x", true},
		{false, root + sep + ".." + sep + "Other", false},
		{false, sep + "Pictures2", false},
		{false, sep, false},
		{false, sep + "pictures" + sep + "Trip", false},
		{true, sep + "pictures" + sep + "Trip", true},
		{false, root + sep + "..foo", true}, // a name that starts with dots is still inside
	} {
		restore := caseInsensitive
		caseInsensitive = tc.insensitive
		if got := Within(tc.p, root); got != tc.want {
			t.Errorf("Within(%q, %q) with case-insensitive=%v = %v, want %v", tc.p, root, tc.insensitive, got, tc.want)
		}
		caseInsensitive = restore
	}
}
