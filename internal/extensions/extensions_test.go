package extensions

import "testing"

func TestClassify_KnownSupportedImage(t *testing.T) {
	for _, p := range []string{"photo.JPG", "photo.jpeg", "photo.heic"} {
		if got := Classify(p); got != Supported {
			t.Errorf("Classify(%q) = %q, want %q", p, got, Supported)
		}
	}
}

func TestClassify_KnownSupportedRaw(t *testing.T) {
	for _, p := range []string{"IMG_1234.CR2", "IMG_1234.dng"} {
		if got := Classify(p); got != Supported {
			t.Errorf("Classify(%q) = %q, want %q", p, got, Supported)
		}
	}
}

func TestClassify_KnownSupportedVideo(t *testing.T) {
	for _, p := range []string{"clip.mp4", "clip.MOV"} {
		if got := Classify(p); got != Supported {
			t.Errorf("Classify(%q) = %q, want %q", p, got, Supported)
		}
	}
}

func TestClassify_KnownUnsupported(t *testing.T) {
	for _, p := range []string{"scan.bmp", "icon.svg", "sidecar.xmp", "slides.ppt", "slides.pptx", "meta.xml", "leftover.arg"} {
		if got := Classify(p); got != Unsupported {
			t.Errorf("Classify(%q) = %q, want %q", p, got, Unsupported)
		}
	}
}

func TestClassify_UnknownExtension(t *testing.T) {
	for _, p := range []string{"mystery.xyz", "no_extension"} {
		if got := Classify(p); got != Unknown {
			t.Errorf("Classify(%q) = %q, want %q", p, got, Unknown)
		}
	}
}

func TestExtOf_LowercasesAndStripsDot(t *testing.T) {
	if got := ExtOf("Photo.JPG"); got != "jpg" {
		t.Errorf("ExtOf = %q, want jpg", got)
	}
	if got := ExtOf("no_extension"); got != "(none)" {
		t.Errorf("ExtOf = %q, want (none)", got)
	}
}

func TestParseKind(t *testing.T) {
	cases := []struct {
		in      string
		want    Kind
		wantErr bool
	}{
		{"", KindUnknown, false},
		{"all", KindUnknown, false},
		{"ALL", KindUnknown, false},
		{"photo", KindPhoto, false},
		{"photos", KindPhoto, false},
		{"Photos", KindPhoto, false},
		{"video", KindVideo, false},
		{"videos", KindVideo, false},
		{"bogus", KindUnknown, true},
	}
	for _, c := range cases {
		got, err := ParseKind(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseKind(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("ParseKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKindOf_BuiltInPhotoAndVideo(t *testing.T) {
	if got := KindOf("photo.jpg"); got != KindPhoto {
		t.Errorf("KindOf(photo.jpg) = %q, want %q", got, KindPhoto)
	}
	if got := KindOf("clip.mp4"); got != KindVideo {
		t.Errorf("KindOf(clip.mp4) = %q, want %q", got, KindVideo)
	}
	if got := KindOf("scan.bmp"); got != KindUnknown {
		t.Errorf("KindOf(scan.bmp) (unsupported) = %q, want %q", got, KindUnknown)
	}
	if got := KindOf("mystery.xyz"); got != KindUnknown {
		t.Errorf("KindOf(mystery.xyz) (unknown) = %q, want %q", got, KindUnknown)
	}
}

// TestApplyUserOverrides_ExtendsAndReplaces is the whole point of the
// config.toml integration: a user-added extension classifies immediately,
// and a fresh call replaces (not accumulates on top of) whatever was
// installed before -- required for tests, and also the correct behavior if
// applyExtensionOverrides ever ran more than once in the same process.
func TestApplyUserOverrides_ExtendsAndReplaces(t *testing.T) {
	t.Cleanup(func() { ApplyUserOverrides(nil, nil, nil) })

	// Not classified at all before any override is installed.
	if got := Classify("clip.insv"); got != Unknown {
		t.Fatalf("precondition failed: Classify(clip.insv) = %q, want %q", got, Unknown)
	}

	ApplyUserOverrides([]string{}, []string{".INSV"}, []string{"TMP", ".arg"})

	if got := Classify("clip.insv"); got != Supported {
		t.Errorf("Classify(clip.insv) = %q, want %q after ApplyUserOverrides", got, Supported)
	}
	if got := KindOf("clip.insv"); got != KindVideo {
		t.Errorf("KindOf(clip.insv) = %q, want %q", got, KindVideo)
	}
	if got := Classify("leftover.tmp"); got != Unsupported {
		t.Errorf("Classify(leftover.tmp) = %q, want %q (case/dot-insensitive match)", got, Unsupported)
	}
	// arg is already built-in unsupported; the extra entry must not break that.
	if got := Classify("weird.arg"); got != Unsupported {
		t.Errorf("Classify(weird.arg) = %q, want %q", got, Unsupported)
	}

	// A second call REPLACES, it doesn't add to, the first.
	ApplyUserOverrides(nil, nil, nil)
	if got := Classify("clip.insv"); got != Unknown {
		t.Errorf("Classify(clip.insv) after clearing overrides = %q, want %q", got, Unknown)
	}
}

// TestApplyUserOverrides_UserConfigWinsOverBuiltIn proves the override
// direction that makes "alter the list" actually mean alter, not just
// append: an extension the built-in table already classifies one way can
// be reclassified by the user, not just extended.
func TestApplyUserOverrides_UserConfigWinsOverBuiltIn(t *testing.T) {
	t.Cleanup(func() { ApplyUserOverrides(nil, nil, nil) })

	// pdf is built-in unsupported; force it supported (photo) via config.
	ApplyUserOverrides([]string{"pdf"}, nil, nil)
	if got := Classify("scan.pdf"); got != Supported {
		t.Errorf("Classify(scan.pdf) = %q, want %q (user override beats built-in unsupported)", got, Supported)
	}

	// jpg is built-in supported; force it unsupported via config.
	ApplyUserOverrides(nil, nil, []string{"jpg"})
	if got := Classify("photo.jpg"); got != Unsupported {
		t.Errorf("Classify(photo.jpg) = %q, want %q (user override beats built-in supported)", got, Unsupported)
	}
}
