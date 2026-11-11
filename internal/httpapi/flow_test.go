package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/httpapi"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/teststore"
)

const partSize = 5 << 20

type api struct {
	t    *testing.T
	srv  *httptest.Server
	pool *pgxpool.Pool
}

func newAPI(t *testing.T) api {
	t.Helper()
	pool := testdb.New(t)
	svc := submissions.New(pool, teststore.New(t), testfix.Queue(t, pool), quiet, submissions.Options{PartSize: partSize})
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{DB: pool, Log: quiet, Submissions: svc}))
	t.Cleanup(srv.Close)
	return api{t: t, srv: srv, pool: pool}
}

// client is one browser: it keeps its own cookies.
type client struct {
	a    api
	http *http.Client
}

func (a api) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{a: a, http: &http.Client{Jar: jar}}
}

// call sends a JSON request and decodes a JSON response into out (if set).
func (c *client) call(method, path string, body any, headers map[string]string, out any) *http.Response {
	c.a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.a.srv.URL+path, r)
	if err != nil {
		c.a.t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.a.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return res
}

func (c *client) login(email string) {
	c.a.t.Helper()
	res := c.call("POST", "/api/auth/login", map[string]string{"email": email, "password": testfix.Password}, nil, nil)
	if res.StatusCode != http.StatusOK {
		c.a.t.Fatalf("login %s: status %d", email, res.StatusCode)
	}
}

type apiError struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func TestLoginSetsAnHttpOnlySessionCookie(t *testing.T) {
	a := newAPI(t)
	userID, email := testfix.User(t, a.pool)
	testfix.Team(t, a.pool, testfix.Hackathon(t, a.pool, testfix.HackathonOpts{}), userID)
	c := a.client()

	var bad apiError
	if res := c.call("POST", "/api/auth/login", map[string]string{"email": email, "password": "wrong"}, nil, &bad); res.StatusCode != http.StatusUnauthorized || bad.Error.Code != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s", res.StatusCode, bad.Error.Code)
	}

	res := c.call("POST", "/api/auth/login", map[string]string{"email": strings.ToUpper(email), "password": testfix.Password}, nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == httpapi.SessionCookie {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie %+v, want HttpOnly and SameSite=Lax", cookie)
	}

	var me struct {
		User  struct{ Email string } `json:"user"`
		Teams []struct{ ID string }  `json:"teams"`
	}
	if res := c.call("GET", "/api/me", nil, nil, &me); res.StatusCode != http.StatusOK || me.User.Email != email || len(me.Teams) != 1 {
		t.Fatalf("me: %d %+v", res.StatusCode, me)
	}

	if res := c.call("POST", "/api/auth/logout", nil, nil, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res := c.call("GET", "/api/me", nil, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d, want 401", res.StatusCode)
	}
}

func TestLoginIsRateLimitedPerEmail(t *testing.T) {
	a := newAPI(t)
	_, email := testfix.User(t, a.pool)
	c := a.client()
	limited := false
	for range 8 {
		res := c.call("POST", "/api/auth/login", map[string]string{"email": email, "password": "guess"}, nil, nil)
		if res.StatusCode == http.StatusTooManyRequests {
			limited = res.Header.Get("Retry-After") != ""
			break
		}
	}
	if !limited {
		t.Fatal("8 wrong passwords in a row were never rate limited")
	}
}

func TestWritesNeedASessionAndJSON(t *testing.T) {
	a := newAPI(t)
	userID, email := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, testfix.Hackathon(t, a.pool, testfix.HackathonOpts{}), userID)

	anon := a.client()
	if res := anon.call("GET", "/api/teams/"+team+"/submission", nil, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d, want 401", res.StatusCode)
	}

	c := a.client()
	c.login(email)
	req, _ := http.NewRequest("POST", a.srv.URL+"/api/teams/"+team+"/versions", strings.NewReader("title=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d, want 415", res.StatusCode)
	}

	var e apiError
	body := map[string]any{"title": "x", "file": map[string]any{"name": "a.pdf", "size": 10, "sha256": "nothex"}, "surprise": true}
	if res := c.call("POST", "/api/teams/"+team+"/versions", body, map[string]string{"Idempotency-Key": "key-123456"}, &e); res.StatusCode != http.StatusBadRequest || e.Error.Code != "bad_json" {
		t.Fatalf("unknown field: %d %s, want 400 bad_json", res.StatusCode, e.Error.Code)
	}
	delete(body, "surprise")
	if res := c.call("POST", "/api/teams/"+team+"/versions", body, map[string]string{"Idempotency-Key": "key-123456"}, &e); res.StatusCode != http.StatusUnprocessableEntity || e.Error.Code != "invalid_input" {
		t.Fatalf("bad hash: %d %s, want 422 invalid_input", res.StatusCode, e.Error.Code)
	}
	if res := c.call("GET", "/api/teams/not-a-uuid/submission", nil, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("bad id: %d, want 404", res.StatusCode)
	}
}

// The whole browser flow over HTTP: sign in, create, retry the create,
// upload parts straight to storage, complete, see it counted.
func TestSubmissionFlowOverHTTP(t *testing.T) {
	a := newAPI(t)
	userID, email := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, testfix.Hackathon(t, a.pool, testfix.HackathonOpts{}), userID)
	c := a.client()
	c.login(email)

	f := testfix.NewFile(partSize + 777)
	body := map[string]any{
		"title": "Rocket", "description": "Ships things", "links": []string{"https://github.com/example/rocket"},
		"file": map[string]any{"name": "rocket.zip", "size": len(f.Body), "sha256": f.SHA256},
	}
	key := map[string]string{"Idempotency-Key": "submit-click-0001"}

	var created struct {
		Version struct {
			ID        string `json:"id"`
			PartCount int    `json:"part_count"`
		} `json:"version"`
	}
	if res := c.call("POST", "/api/teams/"+team+"/versions", body, key, &created); res.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", res.StatusCode)
	}
	var replay struct {
		Version struct{ ID string } `json:"version"`
	}
	res := c.call("POST", "/api/teams/"+team+"/versions", body, key, &replay)
	if res.StatusCode != http.StatusOK || res.Header.Get("Idempotent-Replayed") != "true" || replay.Version.ID != created.Version.ID {
		t.Fatalf("retried create: %d replayed=%q id=%s", res.StatusCode, res.Header.Get("Idempotent-Replayed"), replay.Version.ID)
	}

	id := created.Version.ID
	var signed struct {
		Parts []struct {
			Number  int               `json:"number"`
			Size    int64             `json:"size"`
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"parts"`
	}
	parts := make([]int, created.Version.PartCount)
	for i := range parts {
		parts[i] = i + 1
	}
	if res := c.call("POST", "/api/versions/"+id+"/parts/sign", map[string]any{"parts": parts}, nil, &signed); res.StatusCode != http.StatusOK || len(signed.Parts) != 2 {
		t.Fatalf("sign: %d with %d parts", res.StatusCode, len(signed.Parts))
	}
	for _, p := range signed.Parts {
		start := int64(p.Number-1) * partSize
		testfix.Put(t, p.Method, p.URL, p.Headers, f.Body[start:start+p.Size])
	}

	var done struct {
		Version struct{ State string } `json:"version"`
	}
	if res := c.call("POST", "/api/versions/"+id+"/complete", nil, nil, &done); res.StatusCode != http.StatusOK || done.Version.State != "checking" {
		t.Fatalf("complete: %d state %q", res.StatusCode, done.Version.State)
	}

	var page struct {
		CountedVersionID string `json:"counted_version_id"`
		Versions         []struct {
			ID     string `json:"id"`
			Counts bool   `json:"counts"`
		} `json:"versions"`
	}
	if res := c.call("GET", "/api/teams/"+team+"/submission", nil, nil, &page); res.StatusCode != http.StatusOK || page.CountedVersionID != id || len(page.Versions) != 1 || !page.Versions[0].Counts {
		t.Fatalf("team page: %d %+v", res.StatusCode, page)
	}

	// Another team's member cannot see it.
	otherID, otherEmail := testfix.User(t, a.pool)
	testfix.Team(t, a.pool, testfix.Hackathon(t, a.pool, testfix.HackathonOpts{}), otherID)
	o := a.client()
	o.login(otherEmail)
	for _, path := range []string{"/api/teams/" + team + "/submission", "/api/versions/" + id + "/parts"} {
		if res := o.call("GET", path, nil, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s as another team: %d, want 404", path, res.StatusCode)
		}
	}
	if res := o.call("POST", fmt.Sprintf("/api/versions/%s/complete", id), nil, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("complete as another team: %d, want 404", res.StatusCode)
	}
}
