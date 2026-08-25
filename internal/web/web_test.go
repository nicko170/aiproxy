package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServesTheShellAtRoot(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "<html") {
		t.Errorf("root did not serve the shell: %.120s", body)
	}
}

func TestServesAnAsset(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/style.css", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
}

func TestUnknownAssetIs404(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil))
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Result().StatusCode)
	}
}

// THE test that matters. A renamed or forgotten asset produces a 404 only in a
// real browser, on a screen nobody opened during development. Walking the
// shell's own references and resolving each against the embed catches it in CI
// instead.
func TestEveryReferencedAssetExists(t *testing.T) {
	shell, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := referencedAssets(string(shell))
	if len(refs) == 0 {
		t.Fatal("found no asset references in index.html — the extractor is broken, not the shell")
	}
	for _, ref := range refs {
		if _, err := assetsFS.ReadFile("assets/" + ref); err != nil {
			t.Errorf("index.html references assets/%s, which is not embedded", ref)
		}
	}
}

// Every JS module the app imports must also resolve. ES module specifiers are
// invisible to the HTML scan above.
func TestEveryImportedModuleExists(t *testing.T) {
	for _, name := range AssetNames() {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		src, err := assetsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		dir := name[:strings.LastIndex(name, "/")+1]
		for _, spec := range importedModules(string(src)) {
			resolved := resolveRelative(dir, spec)
			if _, err := assetsFS.ReadFile(resolved); err != nil {
				t.Errorf("%s imports %q, which resolves to %s and is not embedded", name, spec, resolved)
			}
		}
	}
}
