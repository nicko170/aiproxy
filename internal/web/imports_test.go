package web

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// internal/web reads through view.Source and nothing below it, so presentation
// can never compute an aggregate of its own or hold a live pointer into proxy
// state. Same rule as internal/tui, same reason (spec §3.1).
func TestWebImportsOnlyTheViewSeam(t *testing.T) {
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
				t.Errorf("%s imports %s: internal/web may import internal/view and nothing below it", f, path)
			}
		}
	}
}
