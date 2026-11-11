package web

import (
	"embed"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

//go:embed assets
var assetsFS embed.FS

// Handler serves the dashboard: the shell at the mount root, static files
// under assets/, and the session exchange.
//
// Routing inside the SPA is hash-based (#/overview), so there is no
// server-side catch-all here and no path under the mount can be confused with
// a proxied upstream path. An unknown path is a 404, which is what the
// reserved prefix promises for everything it does not recognise.
func Handler(sessions *Sessions, apiKey string) http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("web: embedded assets missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	signIn := SignInHandler(sessions, apiKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/session", signIn)
	mux.Handle("/assets/", cacheHeaders(http.StripPrefix("/assets/", files)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The shell is never cached: it is the one file whose contents change
		// meaning when the binary is updated, and a stale shell loading new
		// assets is the confusing half-broken state.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := assetsFS.ReadFile("assets/index.html")
		w.Write(b)
	})
	return mux
}

// cacheHeaders lets a browser reuse assets within a session without pinning a
// stale copy across an update. no-cache means "revalidate", not "do not
// store": the browser keeps the file and asks whether it changed, which over
// loopback costs nothing and over a tunnel costs one conditional request.
func cacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// AssetNames lists every embedded file, for tests that walk the asset set.
func AssetNames() []string {
	var out []string
	fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

var (
	hrefSrc  = regexp.MustCompile(`(?:src|href)="(?:\./)?(assets/)?([^":]+?)"`)
	importRe = regexp.MustCompile(`(?m)^\s*import\s[^'"]*['"]([^'"]+)['"]`)
)

// referencedAssets extracts local asset paths from the shell, ignoring
// absolute URLs, anything with a scheme, and in-page hash routes (the SPA's
// nav uses href="#/overview" for real anchor semantics — keyboard focus, a
// visible status-bar URL, no-JS fallback affordance — and that is a route,
// never an asset).
func referencedAssets(html string) []string {
	var out []string
	for _, m := range hrefSrc.FindAllStringSubmatch(html, -1) {
		ref := m[2]
		if ref == "" || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "#") || strings.Contains(ref, "//") {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func importedModules(src string) []string {
	var out []string
	for _, m := range importRe.FindAllStringSubmatch(src, -1) {
		if strings.HasPrefix(m[1], ".") {
			out = append(out, m[1])
		}
	}
	return out
}

// resolveRelative joins an ES module specifier against the importing file's
// directory, collapsing the ../ segments a screens/ module uses to reach up.
func resolveRelative(dir, spec string) string {
	parts := strings.Split(strings.TrimSuffix(dir, "/"), "/")
	for _, seg := range strings.Split(spec, "/") {
		switch seg {
		case ".", "":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
