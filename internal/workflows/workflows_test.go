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

// jobBlock returns the text of one job: from its "  name:" line (two-space
// indent, directly under jobs:) up to the next job or the end of the file.
func jobBlock(t *testing.T, text, name string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if l == "  "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no job %q", name)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`).MatchString(lines[i]) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// #91: every released file is attested. The files are the ones the
// Checksums step lists -- an explicit set, not `gpsync-*`, because dist/ also
// holds the raw gpsync-tray.exe -- so the attested set is compared with that
// list, and a file type added to one and not the other fails here.
func TestRelease_AttestsExactlyThePublishedAssets(t *testing.T) {
	release := jobBlock(t, workflowFiles(t)["release.yml"], "release")

	ls := regexp.MustCompile(`ls -1 ([^>\n]+?)\s*> assets\.txt`).FindStringSubmatch(release)
	if ls == nil {
		t.Fatal("can't find the Checksums step's `ls -1 ... > assets.txt` asset list")
	}
	want := map[string]bool{"dist/checksums.txt": true}
	for _, g := range strings.Fields(ls[1]) {
		want["dist/"+g] = true
	}

	step := regexp.MustCompile(`(?s)- name: Attest the release assets\n(.*?)(?:\n      - |\z)`).FindStringSubmatch(release)
	if step == nil {
		t.Fatal("no `Attest the release assets` step in the release job")
	}
	body := step[1]
	if !strings.Contains(body, "uses: actions/attest-build-provenance@") {
		t.Error("the attest step doesn't use actions/attest-build-provenance")
	}
	if !strings.Contains(body, "if: startsWith(github.ref, 'refs/tags/')") {
		t.Error("the attest step must run only on a tag: attestations go into a permanent public log, which a branch's dry run must not write to")
	}
	// The list is the lines indented under `subject-path: |`; what follows
	// it up to the next step is that step's comments.
	got := map[string]bool{}
	inList := false
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.TrimSpace(l) == "subject-path: |":
			inList = true
		case inList && strings.HasPrefix(l, "            ") && strings.TrimSpace(l) != "":
			got[strings.TrimSpace(l)] = true
		case inList:
			inList = false
		}
	}
	if len(got) == 0 {
		t.Fatal("the attest step has no subject-path list")
	}
	for w := range want {
		if !got[w] {
			t.Errorf("%s is published but not attested", w)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("%s is attested but isn't a published asset", g)
		}
	}

	// After publishing and verifying the asset set, so a failure here can't
	// block or half-publish a release; the verify step comes last.
	i := func(s string) int { return strings.Index(release, s) }
	order := []string{"- name: Publish draft release", "- name: Verify the published asset set", "- name: Attest the release assets", "- name: Verify each asset's attestation"}
	for k := 1; k < len(order); k++ {
		if i(order[k-1]) < 0 || i(order[k]) < 0 || i(order[k-1]) > i(order[k]) {
			t.Errorf("%q must come before %q", order[k-1], order[k])
		}
	}
	verify := release[i("- name: Verify each asset's attestation"):]
	for _, s := range []string{"gh attestation verify", `--repo "${{ github.repository }}"`, "if: startsWith(github.ref, 'refs/tags/')", "assets.txt", "checksums.txt"} {
		if !strings.Contains(verify, s) {
			t.Errorf("the attestation verify step lacks %q", s)
		}
	}
}

// Signing needs id-token: write, and storing the result attestations: write.
// Only the job that attests holds them; the build and test jobs run code from
// dependencies and must not be able to mint a signed statement.
func TestWorkflows_OnlyTheReleaseJobCanSign(t *testing.T) {
	for name, text := range workflowFiles(t) {
		for n, l := range codeLines(text) {
			if strings.Contains(l, "id-token:") || strings.Contains(l, "attestations:") {
				if name != "release.yml" {
					t.Errorf("%s:%d grants %s outside release.yml", name, n, strings.TrimSpace(l))
				}
			}
		}
	}
	text := workflowFiles(t)["release.yml"]
	release := jobBlock(t, text, "release")
	for _, perm := range []string{"id-token: write", "attestations: write"} {
		if strings.Count(release, perm) != 1 {
			t.Errorf("the release job must grant %q exactly once", perm)
		}
		if strings.Count(text, perm) != 1 {
			t.Errorf("%q appears %d times in release.yml; only the release job may hold it", perm, strings.Count(text, perm))
		}
	}
}
