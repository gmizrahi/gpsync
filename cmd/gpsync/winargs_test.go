package main

import (
	"reflect"
	"testing"
)

// TestRepairWindowsArgs_TrailingBackslash reproduces the failure where
// `gpsync upload 'C:\Photos\2023\2023_04 Trip\'` from Windows PowerShell
// 5.1 reported "Nothing pending", using the exact argument shapes a probe
// exe received and the raw command lines PowerShell built.
func TestRepairWindowsArgs_TrailingBackslash(t *testing.T) {
	cases := []struct {
		name   string
		parsed []string // what Go's os.Args[1:] contained
		raw    string   // GetCommandLineW
		want   []string
	}{
		{
			name:   "one path with a space and a trailing backslash",
			parsed: []string{"upload", `C:\Photos\2026\2026_09_at_the_park"`},
			raw:    `C:\GPSync\gpsync.exe upload "C:\Photos\2026\2026_09_at_the_park\"`,
			want:   []string{"upload", `C:\Photos\2026\2026_09_at_the_park\`},
		},
		{
			// The case that rules out just stripping a stray quote: the
			// arguments after the broken one were merged into it.
			name:   "several paths, the first one broken",
			parsed: []string{"mark-synced", `C:\Photos\2026\2026_09_at_the_park" C:\NoSpace\ C:\Photos\2023\2023_04`, "Trip"},
			raw:    `"C:\GPSync\gpsync.exe" mark-synced "C:\Photos\2026\2026_09_at_the_park\" C:\NoSpace\ "C:\Photos\2026\2026_09_at_the_park"`,
			want:   []string{"mark-synced", `C:\Photos\2026\2026_09_at_the_park\`, `C:\NoSpace\`, `C:\Photos\2026\2026_09_at_the_park`},
		},
		{
			name:   "flags around the broken path",
			parsed: []string{"upload", "--media-type", "videos", `C:\A B"`},
			raw:    `gpsync.exe upload --media-type videos "C:\A B\"`,
			want:   []string{"upload", "--media-type", "videos", `C:\A B\`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := repairWindowsArgs(c.parsed, c.raw)
			if !ok {
				t.Fatal("repair did not trigger, but an argument contains a double quote")
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestRepairWindowsArgs_LeavesNormalArgsAlone: the repair must only ever
// engage on an argument that contains a double quote. Every normal
// invocation, including a trailing backslash with no space (which
// PowerShell does not quote, so it arrives intact), keeps Go's own parsing.
func TestRepairWindowsArgs_LeavesNormalArgsAlone(t *testing.T) {
	for _, args := range [][]string{
		{"upload", `C:\Photos\2026\2026_09_at_the_park`},
		{"upload", `C:\NoSpace\`},
		{"info"},
		{},
	} {
		got, ok := repairWindowsArgs(args, `gpsync.exe something "completely different"`)
		if ok || !reflect.DeepEqual(got, args) {
			t.Errorf("repairWindowsArgs(%q) = %q, %v -- normal args must pass through untouched", args, got, ok)
		}
	}
	// And with no raw command line available (every non-Windows build),
	// nothing changes even if an argument does contain a quote.
	odd := []string{"upload", `C:\x"`}
	if got, ok := repairWindowsArgs(odd, ""); ok || !reflect.DeepEqual(got, odd) {
		t.Errorf("with no raw command line, args must pass through; got %q, %v", got, ok)
	}
}

func TestSplitCommandLineLiteralBackslashes(t *testing.T) {
	for raw, want := range map[string][]string{
		`a b  c`:                 {"a", "b", "c"},
		`"a b" c`:                {"a b", "c"},
		`x "" y`:                 {"x", "", "y"},
		`p "C:\dir with space\"`: {"p", `C:\dir with space\`},
		"a\tb":                   {"a", "b"},
		`  lead trail  `:         {"lead", "trail"},
	} {
		if got := splitCommandLineLiteralBackslashes(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("split(%q) = %q, want %q", raw, got, want)
		}
	}
}
