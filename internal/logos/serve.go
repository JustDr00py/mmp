package logos

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Handler serves the logo files under URLPrefix to anyone (players fetch
// channel logos without signing in). Only image files in library/ and
// uploads/ are served; no listings, nothing hidden.
func (l *Library) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p, ok := strings.CutPrefix(r.URL.Path, URLPrefix)
		ctype := imageTypes[strings.ToLower(path.Ext(p))]
		if !ok || ctype == "" || !servable(p) {
			http.NotFound(w, r)
			return
		}
		root, err := os.OpenRoot(l.Dir) // no way out through symlinks or ..
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer root.Close()
		f, err := root.Open(filepath.FromSlash(p))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("Cache-Control", "public, max-age=86400")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("X-Content-Type-Options", "nosniff")
		// SVGs can carry scripts; served from MMP's own origin they must
		// not run (an uploaded one would otherwise reach the admin UI).
		h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
		http.ServeContent(w, r, "", st.ModTime(), f)
	})
}

// servable reports whether p names a file in library/ or uploads/ with no
// hidden or empty path parts.
func servable(p string) bool {
	parts := strings.Split(p, "/")
	if len(parts) < 2 || (parts[0] != libraryDir && parts[0] != uploadsDir) {
		return false
	}
	for _, s := range parts {
		if s == "" || strings.HasPrefix(s, ".") || strings.Contains(s, `\`) {
			return false
		}
	}
	return true
}
