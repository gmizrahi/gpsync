package pathx

import "testing"

// TestDisplaySplit_HandlesBothSeparatorsOnAnyHost covers the case that made
// these exist: a Windows path rendered by a dashboard running on Linux. With
// filepath.Dir there, the whole path came back as the file name and the
// folder column showed ".".
func TestDisplaySplit_HandlesBothSeparatorsOnAnyHost(t *testing.T) {
	for _, tc := range []struct {
		path     string
		wantDir  string
		wantName string
	}{
		{`C:\Photos\2026\2026_09 Late Summer\IMG_0001.jpg`, `C:\Photos\2026\2026_09 Late Summer`, "IMG_0001.jpg"},
		{`C:\Photos\IMG_0001.jpg`, `C:\Photos`, "IMG_0001.jpg"},
		{"/home/me/Photos/2026/IMG_0001.jpg", "/home/me/Photos/2026", "IMG_0001.jpg"},
		{"/IMG_0001.jpg", "/", "IMG_0001.jpg"},
		// A bare name has no folder to show.
		{"IMG_0001.jpg", ".", "IMG_0001.jpg"},
		{"", ".", ""},
		// A folder name containing a space, and a mixed-separator path of the
		// kind a hand-edited config can produce.
		{`C:\Photos\Old Town/IMG_2.jpg`, `C:\Photos\Old Town`, "IMG_2.jpg"},
	} {
		if got := DisplayDir(tc.path); got != tc.wantDir {
			t.Errorf("DisplayDir(%q) = %q, want %q", tc.path, got, tc.wantDir)
		}
		if got := DisplayName(tc.path); got != tc.wantName {
			t.Errorf("DisplayName(%q) = %q, want %q", tc.path, got, tc.wantName)
		}
	}
}
