package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/simulate"
)

// withOrganizer runs h only for organizers. Everyone else gets 404, so the
// dashboard's existence is not advertised.
func (s *Server) withOrganizer(h userHandler) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request, u auth.User) {
		if !u.IsOrganizer {
			writeError(w, http.StatusNotFound, "not_found", "not found", nil)
			return
		}
		h(w, r, u)
	})
}

func (s *Server) handleOrganizerStats(w http.ResponseWriter, r *http.Request, _ auth.User) {
	st, err := s.Organizer.Stats(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleOrganizerEvents streams every team's changes; the dashboard
// refetches its counts on each one.
func (s *Server) handleOrganizerEvents(w http.ResponseWriter, r *http.Request, u auth.User) {
	s.stream(w, r, u, "")
}

// handleLatency reports server-side latency per endpoint since the last
// reset: time spent in this server, without the client's network.
func (s *Server) handleLatency(w http.ResponseWriter, _ *http.Request, _ auth.User) {
	since, routes, all := s.latency.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"since": since, "overall": all, "routes": routes})
}

func (s *Server) handleLatencyReset(w http.ResponseWriter, _ *http.Request, _ auth.User) {
	s.latency.reset()
	w.WriteHeader(http.StatusNoContent)
}

// handleSimulate starts a demo rush of synthetic teams in the background.
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if s.Simulator == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "simulation is off on this server", nil)
		return
	}
	st, err := s.Simulator.Start(r.Context())
	switch {
	case errors.Is(err, simulate.ErrBusy):
		writeError(w, http.StatusConflict, "busy", err.Error(), map[string]any{"status": st})
	case errors.Is(err, simulate.ErrLimit):
		writeError(w, http.StatusConflict, "limit", err.Error(), nil)
	case err != nil:
		s.writeServiceError(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, st)
	}
}

func (s *Server) handleSimulateStatus(w http.ResponseWriter, _ *http.Request, _ auth.User) {
	if s.Simulator == nil {
		writeJSON(w, http.StatusOK, simulate.Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.Simulator.Status())
}

func (s *Server) handleOrganizerCSV(w http.ResponseWriter, r *http.Request, _ auth.User) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="submissions-%s.csv"`, time.Now().UTC().Format("20060102-150405")))
	if err := s.Organizer.WriteCSV(r.Context(), w); err != nil {
		// Headers are already sent; the truncated file plus the log is all we can do.
		s.Log.ErrorContext(r.Context(), "csv export", "err", err)
	}
}
