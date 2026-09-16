package main

import (
	"bytes"
	"strings"
	"testing"
)

func renderHelp(t *testing.T, args ...string) string {
	t.Helper()
	root := rootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--help"))
	if err := root.Execute(); err != nil {
		t.Fatalf("gpsync %s --help: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

// Flags with a shorthand (-h, --help) come before long-only flags, each group
// alphabetical. Cobra's default put -h, --help between --all and --missing.
func TestHelp_ShorthandFlagsListedFirst(t *testing.T) {
	cases := []struct {
		args  []string
		order []string
	}{
		{[]string{"recheck"}, []string{"-h, --help", "--all", "--missing", "--retry", "--revalidate", "--unsupported"}},
		{[]string{"scan"}, []string{"-h, --help", "--concurrency", "--media-type"}},
		{[]string{"pending"}, []string{"-h, --help", "-s, --summary"}},
		{nil, []string{"-h, --help", "-v, --version"}},
	}
	for _, c := range cases {
		out := renderHelp(t, c.args...)
		flags := out[strings.Index(out, "\nFlags:"):]
		lines := strings.Split(flags, "\n")
		// Match the start of a flag's own line only: descriptions can name
		// other flags ("Same as --missing --unsupported --retry").
		lineOf := func(flag string) int {
			for i, l := range lines {
				if strings.HasPrefix(strings.TrimSpace(l), flag+" ") {
					return i
				}
			}
			return -1
		}
		last := -1
		for _, want := range c.order {
			i := lineOf(want)
			if i < 0 {
				t.Fatalf("gpsync %v --help: %q not listed:\n%s", c.args, want, flags)
			}
			if i < last {
				t.Errorf("gpsync %v --help: %q is out of order:\n%s", c.args, want, flags)
			}
			last = i
		}
	}
}
