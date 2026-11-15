// Package httpapi serves the JSON API.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/events"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/organizer"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/ratelimit"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

// Deps are what the API needs to run.
type Deps struct {
	DB          *pgxpool.Pool
	Log         *slog.Logger
	Submissions *submissions.Service
	Events      *events.Hub
	Organizer   organizer.Service
	// UI is the built React app (nil when the binary has no UI).
	UI fs.FS
	// StorageOrigin is where browsers send parts; the UI's CSP allows it.
	StorageOrigin string
	// SecureCookies marks the session cookie Secure (HTTPS only).
	SecureCookies bool
	// TrustProxy takes the client IP from the last X-Forwarded-For entry,
	// which Render's proxy appends. Off in development.
	TrustProxy bool
}

// Server holds the API's dependencies and rate limiters.
type Server struct {
	Deps
	loginByIP    *ratelimit.Limiter
	loginByEmail *ratelimit.Limiter
	writes       *ratelimit.Limiter
	reads        *ratelimit.Limiter
	streams      streamCounter
	latency      latency
}

// New returns the API as an http.Handler with all middleware applied.
func New(d Deps) http.Handler {
	s := &Server{
		Deps:         d,
		loginByIP:    ratelimit.New(6*time.Second, 10), // 10 at once, then 10 a minute
		loginByEmail: ratelimit.New(12*time.Second, 5), // 5 at once, then 5 a minute
		writes:       ratelimit.New(100*time.Millisecond, 60),
		reads:        ratelimit.New(50*time.Millisecond, 120),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/time", s.handleTime)
	mux.HandleFunc("GET /api/hackathon", s.handleHackathon)

	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.withUser(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.withUser(s.handleMe))

	mux.HandleFunc("GET /api/teams/{teamID}/submission", s.withUser(s.handleTeamSubmission))
	mux.HandleFunc("POST /api/teams/{teamID}/versions", s.withUser(s.handleCreateVersion))
	mux.HandleFunc("GET /api/versions/{versionID}/parts", s.withUser(s.handleUploadedParts))
	mux.HandleFunc("POST /api/versions/{versionID}/parts/sign", s.withUser(s.handleSignParts))
	mux.HandleFunc("POST /api/versions/{versionID}/complete", s.withUser(s.handleComplete))
	mux.HandleFunc("GET /api/versions/{versionID}/download", s.withUser(s.handleDownload))
	mux.HandleFunc("GET /api/versions/{versionID}/preview", s.withUser(s.handlePreview))
	mux.HandleFunc("GET /api/teams/{teamID}/events", s.withUser(s.handleTeamEvents))

	mux.HandleFunc("GET /api/organizer/stats", s.withOrganizer(s.handleOrganizerStats))
	mux.HandleFunc("GET /api/organizer/events", s.withOrganizer(s.handleOrganizerEvents))
	mux.HandleFunc("GET /api/organizer/export.csv", s.withOrganizer(s.handleOrganizerCSV))
	mux.HandleFunc("GET /api/organizer/latency", s.withOrganizer(s.handleLatency))
	mux.HandleFunc("POST /api/organizer/latency/reset", s.withOrganizer(s.handleLatencyReset))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
	})
	mux.Handle("/", s.uiHandler()) // "/api/" above is more specific and wins

	return s.recoverPanics(s.requestID(s.accessLog(securityHeaders(requireJSON(mux)))))
}

// handleHealth reports whether the server can reach Postgres.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		s.Log.WarnContext(r.Context(), "health check: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

// handleTime returns Postgres's clock, the only clock the deadline is judged
// by. The browser uses it to correct its countdown.
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var now time.Time
	if err := s.DB.QueryRow(ctx, "select now()").Scan(&now); err != nil {
		s.Log.ErrorContext(r.Context(), "read server time", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "server time unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"now": now.UTC().Format(time.RFC3339Nano)})
}

// handleHackathon describes the current hackathon for the sign-in page.
func (s *Server) handleHackathon(w http.ResponseWriter, r *http.Request) {
	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		submissions.Window
	}
	var graceSeconds int
	err := s.DB.QueryRow(r.Context(), `
		select id::text, name, deadline_at, grace_seconds, now()
		from hackathons order by created_at desc limit 1`).
		Scan(&out.ID, &out.Name, &out.DeadlineAt, &graceSeconds, &out.ServerTime)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no hackathon is set up", nil)
		return
	}
	out.GraceEndsAt = out.DeadlineAt.Add(time.Duration(graceSeconds) * time.Second)
	writeJSON(w, http.StatusOK, out)
}

type errorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Details = details
	writeJSON(w, status, body)
}

// statusFor maps a rule violation to its HTTP status.
var statusFor = map[string]int{
	submissions.CodeNotFound:            http.StatusNotFound,
	submissions.CodeInvalidInput:        http.StatusUnprocessableEntity,
	submissions.CodeIdempotencyMismatch: http.StatusUnprocessableEntity,
	submissions.CodeFileTooLarge:        http.StatusUnprocessableEntity,
	submissions.CodeDeadlinePassed:      http.StatusConflict,
	submissions.CodeGraceOver:           http.StatusConflict,
	submissions.CodeWrongState:          http.StatusConflict,
	submissions.CodePartsMissing:        http.StatusConflict,
	submissions.CodeUploadMismatch:      http.StatusConflict,
	submissions.CodeVersionLimit:        http.StatusConflict,
	submissions.CodeStorageFull:         http.StatusConflict,
	submissions.CodeUploadsDisabled:     http.StatusServiceUnavailable,
	submissions.CodeStorageUnavailable:  http.StatusServiceUnavailable,
}

// writeServiceError reports a service error, hiding internal failures.
func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var e *submissions.Error
	if errors.As(err, &e) {
		status, ok := statusFor[e.Code]
		if !ok {
			status = http.StatusBadRequest
		}
		writeError(w, status, e.Code, e.Message, e.Details)
		return
	}
	s.Log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "something went wrong; retry", nil)
}

// decodeJSON reads one JSON object into dst, refusing unknown fields,
// trailing data and bodies over 64 KiB.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "the request body is not valid JSON for this endpoint", map[string]any{"reason": err.Error()})
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_json", "the request body must be a single JSON object", nil)
		return false
	}
	return true
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// pathID returns a UUID path value, or writes 404 and returns "".
func pathID(w http.ResponseWriter, r *http.Request, name string) string {
	id := r.PathValue(name)
	if !uuidRE.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "not found", nil)
		return ""
	}
	return strings.ToLower(id)
}

// clientIP is the caller's address, from the proxy header when trusted.
func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; wait a few seconds", nil)
}

type ctxKey int

const requestIDKey ctxKey = 0

// requestID tags every request with an id, echoed in X-Request-ID and logs.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer (needed for SSE).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.latency.observe(r, time.Since(start))
		id, _ := r.Context().Value(requestIDKey).(string)
		s.Log.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", id)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.ErrorContext(r.Context(), "panic in handler", "panic", v, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// requireJSON refuses API writes that are not JSON. A cross-site HTML form
// cannot send application/json, so together with SameSite cookies this
// blocks cross-site request forgery.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mt != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send Content-Type: application/json", nil)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
