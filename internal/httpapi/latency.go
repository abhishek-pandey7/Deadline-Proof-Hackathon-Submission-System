package httpapi

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// latency keeps a per-route histogram of how long the server took, from the
// first byte of the request to the last of the response. It measures our
// code, not the network between the client and the server, which a load test
// run from far away also includes.
type latency struct {
	mu     sync.Mutex
	since  time.Time
	routes map[string]*histogram
}

// Bucket upper bounds in milliseconds.
var bucketsMS = []float64{1, 2, 5, 10, 15, 20, 30, 40, 50, 75, 100, 150, 200, 250, 300, 400, 500, 750, 1000, 2000, 5000}

type histogram struct {
	counts []int64 // len(bucketsMS)+1, the last is overflow
	n      int64
	sumMS  float64
	maxMS  float64
}

var idSegment = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// routeKey groups requests by endpoint, with ids replaced.
func routeKey(r *http.Request) string {
	return r.Method + " " + idSegment.ReplaceAllString(r.URL.Path, "{id}")
}

func (l *latency) observe(r *http.Request, d time.Duration) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") || strings.HasSuffix(p, "/events") {
		return // only API calls; event streams stay open by design
	}
	ms := float64(d.Microseconds()) / 1000
	key := routeKey(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.routes == nil {
		l.routes, l.since = map[string]*histogram{}, time.Now()
	}
	h := l.routes[key]
	if h == nil {
		h = &histogram{counts: make([]int64, len(bucketsMS)+1)}
		l.routes[key] = h
	}
	i := sort.SearchFloat64s(bucketsMS, ms)
	h.counts[i]++
	h.n++
	h.sumMS += ms
	h.maxMS = max(h.maxMS, ms)
}

// quantile returns the bucket upper bound below which q of requests fell.
func (h *histogram) quantile(q float64) float64 {
	target := int64(q*float64(h.n) + 0.999999)
	var seen int64
	for i, c := range h.counts {
		seen += c
		if seen >= target {
			if i < len(bucketsMS) {
				return bucketsMS[i]
			}
			return h.maxMS
		}
	}
	return h.maxMS
}

// RouteLatency summarises one endpoint.
type RouteLatency struct {
	Route  string  `json:"route"`
	Count  int64   `json:"count"`
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

func (l *latency) snapshot() (time.Time, []RouteLatency, RouteLatency) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all := &histogram{counts: make([]int64, len(bucketsMS)+1)}
	out := make([]RouteLatency, 0, len(l.routes))
	for k, h := range l.routes {
		out = append(out, summarize(k, h))
		for i, c := range h.counts {
			all.counts[i] += c
		}
		all.n += h.n
		all.sumMS += h.sumMS
		all.maxMS = max(all.maxMS, h.maxMS)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return l.since, out, summarize("all API requests", all)
}

func summarize(route string, h *histogram) RouteLatency {
	if h.n == 0 {
		return RouteLatency{Route: route}
	}
	return RouteLatency{Route: route, Count: h.n, MeanMS: h.sumMS / float64(h.n),
		P50MS: h.quantile(0.50), P95MS: h.quantile(0.95), P99MS: h.quantile(0.99), MaxMS: h.maxMS}
}

func (l *latency) reset() {
	l.mu.Lock()
	l.routes, l.since = map[string]*histogram{}, time.Now()
	l.mu.Unlock()
}
