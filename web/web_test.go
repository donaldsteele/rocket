package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestAssetsPresent: every file the pages load is embedded.
func TestAssetsPresent(t *testing.T) {
	for _, name := range []string{
		"index.html", "drop.html", "app.js", "drop.js", "styles.css", "theme.js", "icons.svg", "favicon.png",
		"vendor/alpine.min.js", "vendor/LICENSE-lucide.txt", "vendor/fonts/Inter-Variable-latin.woff2",
	} {
		if _, err := fs.Stat(FS(), name); err != nil {
			t.Errorf("%s not embedded: %v", name, err)
		}
	}
	for _, page := range []string{"index.html", "drop.html"} {
		b, err := fs.ReadFile(FS(), page)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`(?:src|href)="([^"#:]+)"`).FindAllStringSubmatch(string(b), -1) {
			if _, err := fs.Stat(FS(), m[1]); err != nil {
				t.Errorf("%s references %s, which is not embedded", page, m[1])
			}
		}
	}
}

// TestIconsDefined: every sprite icon the pages and scripts name exists in
// icons.svg. Dynamic names are listed as icon: '<name>' in the scripts.
func TestIconsDefined(t *testing.T) {
	sprite, err := fs.ReadFile(FS(), "icons.svg")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="i-([a-z0-9-]+)"`).FindAllStringSubmatch(string(sprite), -1) {
		defined[m[1]] = true
	}
	refs := []*regexp.Regexp{
		regexp.MustCompile(`icons\.svg#i-([a-z0-9-]+)`),
		regexp.MustCompile(`icon: '([a-z0-9-]+)'`),
	}
	used := 0
	for _, name := range []string{"index.html", "drop.html", "app.js", "drop.js"} {
		b, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range refs {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				used++
				if !defined[m[1]] {
					t.Errorf("%s uses icon %q, which icons.svg does not define", name, m[1])
				}
			}
		}
	}
	if used < 10 {
		t.Fatalf("found only %d icon references; the patterns are stale", used)
	}
}

// TestDropPageStandalone: the LAN drop page loads nothing that is loopback
// only (Alpine, app.js), so it works for guests.
func TestDropPageStandalone(t *testing.T) {
	b, err := fs.ReadFile(FS(), "drop.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"alpine", "app.js", "x-data"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("drop.html mentions %q", bad)
		}
	}
}
