// Package events fans Postgres notifications out to live browser streams.
//
// Every state change runs pg_notify inside the transaction that makes it, so
// Postgres delivers it only once the change is committed. One connection
// per server LISTENs and hands each event to the streams that care: the
// team's members and the organizers. A stream treats any event as "refetch",
// so dropping an event when a slow stream's buffer is full loses nothing.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event is one change. Kind is "version" for a state change and "resync"
// after the listener reconnected and events may have been missed.
type Event struct {
	Kind      string `json:"kind"`
	VersionID string `json:"version_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	State     string `json:"state,omitempty"`
}

// Sub is one live stream's subscription.
type Sub struct {
	C      chan Event
	teamID string // "" receives every team's events (organizers)
}

// Hub owns the LISTEN connection and the subscriptions.
type Hub struct {
	connString string
	channel    string
	log        *slog.Logger

	mu    sync.Mutex
	subs  map[*Sub]struct{}
	ready chan struct{}
	once  sync.Once

	reconnects atomic.Int64
	listenPID  atomic.Uint32
}

// NewHub listens on channel using its own connection to connString, which
// must be a direct (not pooled) connection: PgBouncer drops LISTEN.
func NewHub(connString, channel string, log *slog.Logger) *Hub {
	return &Hub{connString: connString, channel: channel, log: log, subs: map[*Sub]struct{}{}, ready: make(chan struct{})}
}

// Ready is closed once the hub first starts listening.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Reconnects counts how often the listener had to reconnect.
func (h *Hub) Reconnects() int64 { return h.reconnects.Load() }

// ListenerPID is the Postgres backend pid of the listening connection.
func (h *Hub) ListenerPID() uint32 { return h.listenPID.Load() }

// Subscribe returns a subscription for one team, or for all teams when
// teamID is "".
func (h *Hub) Subscribe(teamID string) *Sub {
	s := &Sub{C: make(chan Event, 16), teamID: teamID}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Unsubscribe stops a subscription.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *Hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if ev.Kind == "resync" || s.teamID == "" || s.teamID == ev.TeamID {
			select {
			case s.C <- ev:
			default: // the stream already has events queued; it will refetch anyway
			}
		}
	}
}

// Run listens until ctx ends, reconnecting with backoff when the connection
// drops. After a reconnect every stream gets a resync event.
func (h *Hub) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		h.reconnects.Add(1)
		h.log.Warn("event listener disconnected; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, h.connString)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "listen "+pgx.Identifier{h.channel}.Sanitize()); err != nil {
		return err
	}
	h.listenPID.Store(conn.PgConn().PID())
	first := false
	h.once.Do(func() { close(h.ready); first = true })
	if !first {
		h.publish(Event{Kind: "resync"}) // events during the outage were missed
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var ev Event
		if json.Unmarshal([]byte(n.Payload), &ev) != nil || ev.TeamID == "" {
			continue
		}
		ev.Kind = "version"
		h.publish(ev)
	}
}
