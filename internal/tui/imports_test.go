package tui

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTUIImportsOnlyTheViewSeam enforces the architecture this package exists
// inside: internal/tui reads everything through view.Source and imports
// nothing below that seam — not account, not metrics, not proxy, not
// provider, not config. This is what lets a future view.HTTP drive this same
// TUI against a detached daemon; an import of a lower package here is a
// defect, not a shortcut.
func TestTUIImportsOnlyTheViewSeam(t *testing.T) {
	const module = "github.com/nicko170/aiproxy/"
	allowed := map[string]bool{
		module + "internal/view": true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; test is looking in the wrong place")
	}
	fset := token.NewFileSet()
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, module) && !allowed[path] {
				t.Errorf("%s imports %s: internal/tui may import internal/view and nothing below it", f, path)
			}
		}
	}
}
