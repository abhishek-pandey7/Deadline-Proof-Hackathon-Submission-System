package httpapi

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// uiHandler serves the single-page app: real files when they exist, and
// index.html for any other path so client-side routes survive a reload.
// Hashed assets are cached for a year; index.html is never cached, so a
// deploy is picked up on the next load.
func (s *Server) uiHandler() http.Handler {
	getOnly := func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			next(w, r)
		})
	}
	if s.UI == nil {
		return getOnly(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "The UI is not part of this build. Run `npm run build` in web/ and build with -tags embedui, or use the Vite dev server.", http.StatusNotFound)
		})
	}
	csp := contentSecurityPolicy(s.StorageOrigin)
	files := http.FileServerFS(s.UI)
	return getOnly(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if _, err := fs.Stat(s.UI, name); err == nil {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			} else if !errors.Is(err, fs.ErrNotExist) || path.Ext(name) != "" {
				http.NotFound(w, r) // a missing asset is a real 404, not the app
				return
			}
		}
		index, err := fs.ReadFile(s.UI, "index.html")
		if err != nil {
			http.Error(w, "index.html missing from the UI build", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", csp)
		_, _ = w.Write(index)
	})
}

// contentSecurityPolicy allows only this origin, plus the storage origin
// the browser uploads parts to and loads previews from.
func contentSecurityPolicy(storageOrigin string) string {
	extra := ""
	if u, err := url.Parse(storageOrigin); err == nil && u.Scheme != "" && u.Host != "" {
		extra = " " + u.Scheme + "://" + u.Host
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; " +
		"img-src 'self' data:" + extra + "; connect-src 'self'" + extra + "; " +
		"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}
