package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
)

// maxStreamsPerUser caps open event streams (several tabs are fine, a
// script opening thousands is not).
const maxStreamsPerUser = 4

// streamHeartbeat keeps proxies from closing an idle stream.
const streamHeartbeat = 20 * time.Second

type streamCounter struct {
	mu   sync.Mutex
	open map[string]int
}

func (c *streamCounter) acquire(userID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open == nil {
		c.open = map[string]int{}
	}
	if c.open[userID] >= maxStreamsPerUser {
		return false
	}
	c.open[userID]++
	return true
}

func (c *streamCounter) release(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[userID] <= 1 {
		delete(c.open, userID)
		return
	}
	c.open[userID]--
}

// handleTeamEvents streams a team's version changes to its members.
func (s *Server) handleTeamEvents(w http.ResponseWriter, r *http.Request, u auth.User) {
	teamID := pathID(w, r, "teamID")
	if teamID == "" {
		return
	}
	var member bool
	if err := s.DB.QueryRow(r.Context(),
		`select exists (select 1 from team_members where team_id = $1 and user_id = $2)`, teamID, u.ID).Scan(&member); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if !member {
		writeError(w, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	s.stream(w, r, u, teamID)
}

// stream writes server-sent events until the client goes away. Every
// event means "something changed: refetch"; the client never trusts an
// event's payload for state, only the API.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, u auth.User, teamID string) {
	if s.Events == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live updates are off", nil)
		return
	}
	if !s.streams.acquire(u.ID) {
		tooMany(w)
		return
	}
	defer s.streams.release(u.ID)

	sub := s.Events.Subscribe(teamID)
	defer s.Events.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	// The server's read timeout would otherwise cancel this request after
	// 30 seconds; a stream lives until the browser leaves.
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to buffer
	w.WriteHeader(http.StatusOK)

	// "ready" tells the client to fetch the current state now; anything
	// that changes after this point arrives as an event.
	if _, err := fmt.Fprint(w, "retry: 3000\nevent: ready\ndata: {}\n\n"); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.C:
			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}
