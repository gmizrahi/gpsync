// Package workflows holds tests of the CI configuration in .github/workflows
// itself. Nothing else checks that a pin stays a pin: a tool installed with
// @latest, or an action referenced by a tag, looks exactly like one that was
// pinned until it changes under the pipeline.
package workflows

import (
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
