package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/httpapi"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
)

func TestUIServesTheAppShellAndCachesOnlyHashedAssets(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/index-abc1.js": {Data: []byte("console.log(1)")},
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{DB: testdb.New(t), Log: quiet, UI: ui, StorageOrigin: "https://acct.r2.cloudflarestorage.com"}))
	defer srv.Close()

	get := func(path string) (*http.Response, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	for _, p := range []string{"/", "/team/1b4e28ba-2fa1-11d2-883f-0016d3cca427", "/organizer"} {
		res, body := get(p)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, "id=root") {
			t.Fatalf("%s: %d %q, want the app shell", p, res.StatusCode, body)
		}
		if res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: Cache-Control %q, the shell must not be cached", p, res.Header.Get("Cache-Control"))
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self' https://acct.r2.cloudflarestorage.com") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, csp)
		}
	}

	res, _ := get("/assets/index-abc1.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed asset: %d cache %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
	if res, _ := get("/assets/missing.js"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset: %d, want 404", res.StatusCode)
	}
	if res, body := get("/api/nope"); res.StatusCode != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("api 404 must stay JSON: %d %q", res.StatusCode, body)
	}
}
