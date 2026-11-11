// Package ratelimit keeps one token bucket per key (a user, an IP, an email).
// State is in memory, which is enough for the single free-tier instance.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter allows Burst requests at once per key, refilled at Every.
type Limiter struct {
	every time.Duration
	burst int

	mu      sync.Mutex
	buckets map[string]*bucket
	sweptAt time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// New returns a limiter that refills one token per every, up to burst.
func New(every time.Duration, burst int) *Limiter {
	return &Limiter{every: every, burst: burst, buckets: map[string]*bucket{}}
}

// Allow reports whether key may make a request now, and uses a token if so.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	l.sweep(now)
	return b.lim.AllowN(now, 1)
}

// sweep drops buckets idle long enough to have refilled completely.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.sweptAt) < time.Minute {
		return
	}
	l.sweptAt = now
	idle := l.every * time.Duration(l.burst)
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idle {
			delete(l.buckets, k)
		}
	}
}
