package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLatencyGroupsRoutesAndReportsPercentiles(t *testing.T) {
	var l latency
	for i := 1; i <= 100; i++ {
		r := httptest.NewRequest("POST", "/api/versions/1b4e28ba-2fa1-11d2-883f-0016d3cca427/complete", nil)
		l.observe(r, time.Duration(i)*time.Millisecond) // 1..100 ms
	}
	l.observe(httptest.NewRequest("GET", "/api/teams/1b4e28ba-2fa1-11d2-883f-0016d3cca427/events", nil), time.Hour)
	l.observe(httptest.NewRequest("GET", "/healthz", nil), time.Second)

	_, routes, all := l.snapshot()
	if len(routes) != 1 || routes[0].Route != "POST /api/versions/{id}/complete" {
		t.Fatalf("routes = %+v, want one complete route (streams and health excluded)", routes)
	}
	r := routes[0]
	if r.Count != 100 || r.P50MS != 50 || r.P95MS != 100 || r.MaxMS != 100 || all.Count != 100 {
		t.Fatalf("summary = %+v, want count 100, p50 50, p95 100 (bucket bounds), max 100", r)
	}
	l.reset()
	if _, routes, _ := l.snapshot(); len(routes) != 0 {
		t.Fatal("reset kept routes")
	}
}
