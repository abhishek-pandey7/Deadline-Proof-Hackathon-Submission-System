package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/httpapi"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newBareServer serves the API without storage, for endpoints that need none.
func newBareServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{DB: testdb.New(t), Log: quiet}))
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, into any) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if into != nil {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res
}

func TestHealthzReportsDatabaseOK(t *testing.T) {
	srv := newBareServer(t)
	var body map[string]string
	res := getJSON(t, srv.URL+"/healthz", &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body["status"] != "ok" || body["database"] != "ok" {
		t.Fatalf("body = %v", body)
	}
	if res.Header.Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID header")
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

func TestTimeComesFromTheDatabaseClock(t *testing.T) {
	srv := newBareServer(t)
	var body struct {
		Now time.Time `json:"now"`
	}
	res := getJSON(t, srv.URL+"/api/time", &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("time must not be cached")
	}
	if d := time.Since(body.Now); d > time.Minute || d < -time.Minute {
		t.Fatalf("server time %v is %v away from the test clock", body.Now, d)
	}
}

func TestUnknownAPIRouteReturnsJSON404(t *testing.T) {
	srv := newBareServer(t)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	res := getJSON(t, srv.URL+"/api/does-not-exist", &body)
	if res.StatusCode != http.StatusNotFound || body.Error.Code != "not_found" {
		t.Fatalf("status %d code %q, want 404 not_found", res.StatusCode, body.Error.Code)
	}
}

func TestHealthzRejectsWrongMethod(t *testing.T) {
	srv := newBareServer(t)
	res, err := http.Post(srv.URL+"/healthz", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}
