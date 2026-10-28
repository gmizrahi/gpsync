package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// #77: each theme token is defined once, as light-dark(light, dark), so a
// colour changes in one place. The explicit Light and Dark choices only pin
// color-scheme, and no palette hides in a media query. That this renders
// the same as the old four copies was checked by screenshot in every state
// (system light/dark, explicit Light/Dark on either system).
func TestTheme_EachTokenDefinedOnceWithLightDark(t *testing.T) {
	css := sharedCSS
	if strings.Contains(css, "prefers-color-scheme") {
		t.Error("a prefers-color-scheme block is back: the palette would be defined twice")
	}
	defs := regexp.MustCompile(`(--[a-z-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(css, -1)
	seen := map[string]int{}
	for _, d := range defs {
		seen[d[1]]++
		if !regexp.MustCompile(`^light-dark\(#[0-9a-f]{6}, #[0-9a-f]{6}\)$`).MatchString(strings.TrimSpace(d[2])) {
			t.Errorf("%s: %s, want light-dark(#light, #dark)", d[1], d[2])
		}
	}
	for _, tok := range []string{"--bg", "--bg-card", "--border", "--text", "--text-dim", "--bg-input", "--track", "--ok", "--warn", "--err", "--info"} {
		if seen[tok] != 1 {
			t.Errorf("%s is defined %d times, want once", tok, seen[tok])
		}
	}
	for _, rule := range []string{
		`:root[data-theme="light"] { color-scheme: light; }`,
		`:root[data-theme="dark"] { color-scheme: dark; }`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("missing %s", rule)
		}
	}
	if !regexp.MustCompile(`:root \{\s*/?\*?[^}]*color-scheme: light dark;`).MatchString(css) {
		t.Error(":root doesn't default to color-scheme: light dark, so the system's choice wouldn't apply")
	}
}
