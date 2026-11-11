package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
)

// SessionCookie is the name of the login cookie.
const SessionCookie = "h4h_session"

type userHandler func(w http.ResponseWriter, r *http.Request, u auth.User)

// sessionToken reads the token from the cookie, or from a Bearer header
// (used by the load test, which logs in ahead of time).
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// withUser runs h only for a signed-in user, and rate-limits per user.
func (s *Server) withUser(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := auth.UserForToken(r.Context(), s.DB, sessionToken(r))
		if errors.Is(err, auth.ErrNoSession) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in first", nil)
			return
		}
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		limiter := s.reads
		if r.Method != http.MethodGet {
			limiter = s.writes
		}
		if !limiter.Allow(u.ID) {
			tooMany(w)
			return
		}
		h(w, r, u)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email := auth.NormalizeEmail(body.Email)
	if email == "" || body.Password == "" || len(email) > 254 || len(body.Password) > 200 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "email and password are required", nil)
		return
	}
	if !s.loginByIP.Allow(s.clientIP(r)) || !s.loginByEmail.Allow(email) {
		tooMany(w)
		return
	}

	token, u, err := auth.Login(r.Context(), s.DB, email, body.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "wrong email or password", nil)
		return
	}
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := auth.Logout(r.Context(), s.DB, sessionToken(r)); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u auth.User) {
	type team struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		HackathonID   string `json:"hackathon_id"`
		HackathonName string `json:"hackathon_name"`
	}
	rows, err := s.DB.Query(r.Context(), `
		select t.id::text, t.name, h.id::text, h.name
		from team_members m join teams t on t.id = m.team_id join hackathons h on h.id = t.hackathon_id
		where m.user_id = $1 order by t.name`, u.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	defer rows.Close()
	teams := []team{}
	for rows.Next() {
		var t team
		if err := rows.Scan(&t.ID, &t.Name, &t.HackathonID, &t.HackathonName); err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "teams": teams})
}
