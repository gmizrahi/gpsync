// Package workflows holds tests of the CI configuration in .github/workflows
// itself. Nothing else checks that a pin stays a pin: a tool installed with
// @latest, or an action referenced by a tag, looks exactly like one that was
// pinned until it changes under the pipeline.
package workflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowFiles returns every workflow's path and text.
func workflowFiles(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	out := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = string(b)
	}
	return out
}

// codeLines returns a file's lines without comments and blank lines, with
// their numbers, so a rule isn't tripped by a comment explaining it.
func codeLines(text string) map[int]string {
	out := map[int]string{}
	for i, l := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
			out[i+1] = l
		}
	}
	return out
}

// #90: a tool installed in CI names its version. `go install ...@latest`
// (or @master/@main) means the gate can change behaviour, or fail to build,
// with nothing in the repository changing.
func TestWorkflows_GoInstallToolsArePinned(t *testing.T) {
	floating := regexp.MustCompile(`go install\s+\S+@(latest|master|main|HEAD)\b`)
	installs := 0
	for name, text := range workflowFiles(t) {
		for n, l := range codeLines(text) {
			if strings.Contains(l, "go install") {
				installs++
			}
			if floating.MatchString(l) {
				t.Errorf("%s:%d installs a tool at a moving version: %s", name, n, strings.TrimSpace(l))
			}
		}
	}
	if installs == 0 {
		t.Error("found no `go install` in any workflow: the rule would pass without checking anything")
	}
}

// #89: every action is referenced by a full commit SHA, with the version it
// is in a trailing comment. A tag can be moved to different code after the
// workflow was reviewed, and release.yml's publishing job holds
// contents: write. The comment is required too: it's what Dependabot's
// github-actions updates read and keep current, so the pins don't go stale.
var (
	usesLine   = regexp.MustCompile(`^\s*-?\s*uses:\s*(\S+)(?:\s+#\s*(.*))?$`)
	pinnedRef  = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`^v\d+\.\d+\.\d+\b`)
)

// usesProblem says what's wrong with one `uses:` value and its trailing
// comment, or "" if it is pinned properly.
func usesProblem(ref, comment string) string {
	if strings.HasPrefix(ref, "./") {
		return "" // a local action lives in this repository
	}
	if !pinnedRef.MatchString(ref) {
		return "pin it to a full commit SHA"
	}
	if !versionTag.MatchString(comment) {
		return "add the version in a trailing comment (# v1.2.3), which Dependabot needs to keep the pin current"
	}
	return ""
}

// actionProblems checks every `uses:` in the given workflows (name -> text).
// Finding none at all is itself a problem: a parse that quietly matched
// nothing would otherwise pass.
func actionProblems(files map[string]string) []string {
	var out []string
	seen := 0
	for name, text := range files {
		for n, l := range codeLines(text) {
			m := usesLine.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			seen++
			if p := usesProblem(m[1], m[2]); p != "" {
				out = append(out, fmt.Sprintf("%s:%d %s: %s", name, n, m[1], p))
			}
		}
	}
	if seen == 0 {
		out = append(out, "found no `uses:` in any workflow: the rule would pass without checking anything")
	}
	return out
}

func TestWorkflows_ActionsArePinnedToACommit(t *testing.T) {
	for _, p := range actionProblems(workflowFiles(t)) {
		t.Error(p)
	}
}

// A rule that can't find anything to check must say so, not pass.
func TestActionProblems_FindingNothingIsAProblem(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no files":            {},
		"a file with no uses": {"x.yml": "name: x\njobs:\n  a:\n    steps:\n      - run: echo hi\n"},
	} {
		if got := actionProblems(files); len(got) != 1 || !strings.Contains(got[0], "found no `uses:`") {
			t.Errorf("%s: %v; want the found-nothing problem", name, got)
		}
	}
	good := map[string]string{"x.yml": "      - uses: actions/checkout@" + strings.Repeat("a1", 20) + " # v7.0.1\n"}
	if got := actionProblems(good); len(got) != 0 {
		t.Errorf("a pinned action: %v", got)
	}
}

// The rule itself: each weak way of referencing an action is refused.
func TestUsesProblem(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	for _, tc := range []struct {
		ref, comment string
		bad          bool
		why          string
	}{
		{"actions/checkout@" + sha, "v7.0.1", false, "a SHA with its version"},
		{"actions/checkout@" + sha, "v7.0.1 (2026-07-17)", false, "extra words after the version"},
		{"github/codeql-action/analyze@" + sha, "v4.39.3", false, "a sub-action path"},
		{"./.github/actions/x", "", false, "a local action"},
		{"actions/checkout@v7", "", true, "a moving major tag"},
		{"actions/checkout@v7.0.1", "", true, "an exact tag, which can still be moved"},
		{"actions/checkout@main", "", true, "a branch"},
		{"actions/checkout@" + sha[:12], "v7.0.1", true, "a short SHA"},
		{"actions/checkout@" + strings.ToUpper(sha), "v7.0.1", true, "an upper-case SHA, which isn't how a commit is named"},
		{"actions/checkout@" + sha, "", true, "no version comment"},
		{"actions/checkout@" + sha, "latest", true, "a comment that isn't a version"},
		{"actions/checkout@" + sha, "v7", true, "a major version only"},
	} {
		if got := usesProblem(tc.ref, tc.comment) != ""; got != tc.bad {
			t.Errorf("%s: %q # %q refused = %v, want %v", tc.why, tc.ref, tc.comment, got, tc.bad)
		}
	}
}
