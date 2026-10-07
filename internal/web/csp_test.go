package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The CSP is default-src 'self': inline styles, scripts and event handlers
// would be blocked in the browser, so they must never reach a template.
func TestTemplatesAreCSPClean(t *testing.T) {
	inline := regexp.MustCompile(`(?i)<style|\sstyle\s*=|\son[a-z]+\s*=`)
	script := regexp.MustCompile(`(?i)<script[^>]*>`)
	files, _ := fs.Glob(assets, "templates/*.html")
	if len(files) == 0 {
		t.Fatal("no templates")
	}
	for _, f := range files {
		b, _ := fs.ReadFile(assets, f)
		if m := inline.Find(b); m != nil {
			t.Errorf("%s: inline style or handler: %q", f, m)
		}
		for _, tag := range script.FindAll(b, -1) {
			if !strings.Contains(string(tag), "src=") {
				t.Errorf("%s: inline script: %q", f, tag)
			}
		}
	}
}
