// Command loadtest runs the "deadline crush": 2,000 teams submit within 10
// minutes, the arrival rate climbing toward the end (60 -> 150 per minute over
// 5 minutes, 150 -> 300 over 3, 300 -> 500 over 2). Each team runs the real
// flow: create a version, sign its part, PUT the bytes straight to storage,
// complete. Like the browser client, it retries network failures, overload
// and storage hiccups with backoff (same idempotency key, re-signing an
// expired URL), and records how many requests needed a retry. It is the same
// scenario as loadtest/submit.js for k6.
//
//	go run ./cmd/loadtest -base https://deadline-crush-q60y.onrender.com -tokens loadtest/tokens.csv
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type stage struct {
	to  float64 // arrivals per minute at the end of the stage
	dur time.Duration
}

var stages = []stage{{150, 5 * time.Minute}, {300, 3 * time.Minute}, {500, 2 * time.Minute}}

const (
	startRate   = 60.0
	maxAttempts = 6
)

type sample struct {
	step    string
	status  int
	ms      float64
	at      time.Duration // since the run started
	err     string
	attempt int
}

type recorder struct {
	mu        sync.Mutex
	start     time.Time
	samples   []sample
	completed int
	failed    map[string]int
}

func (r *recorder) add(s sample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

func (r *recorder) outcome(ok bool, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.completed++
		return
	}
	r.failed[why]++
}

func makeFile(size int) []byte {
	b := make([]byte, size)
	copy(b, "%PDF-1.4\n% load test\n")
	copy(b[size-7:], "\n%%EOF\n")
	return b
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "server URL")
	tokensPath := flag.String("tokens", "loadtest/tokens.csv", "team_id,token CSV from cmd/seed")
	skip := flag.Int("skip", 3, "skip the first teams (the demo accounts)")
	teams := flag.Int("teams", 2000, "submissions to make")
	scale := flag.Float64("time-scale", 1, "shrink the 10 minutes, e.g. 0.1 for a 1-minute dry run")
	sizeList := flag.String("sizes", "65536", "comma-separated file sizes in bytes, cycled per team")
	out := flag.String("out", "loadtest/results", "folder for results.json")
	flag.Parse()

	var sizes []int
	for _, s := range strings.Split(*sizeList, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 64 {
			fail(fmt.Errorf("bad size %q", s))
		}
		sizes = append(sizes, n)
	}

	f, err := os.Open(*tokensPath)
	if err != nil {
		fail(err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		fail(err)
	}
	rows = rows[1+*skip:]
	if len(rows) < *teams {
		fail(fmt.Errorf("%s has %d load-test teams, need %d", *tokensPath, len(rows), *teams))
	}

	files := map[int][]byte{}
	hashes := map[int]string{}
	for _, s := range sizes {
		files[s] = makeFile(s)
		sum := sha256.Sum256(files[s])
		hashes[s] = hex.EncodeToString(sum[:])
	}

	client := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{MaxIdleConns: 1000, MaxIdleConnsPerHost: 500, IdleConnTimeout: 90 * time.Second},
	}

	// Wake a sleeping free instance before the clock starts.
	if res, err := client.Get(*base + "/healthz"); err != nil || res.StatusCode != 200 {
		fail(fmt.Errorf("server not healthy: %v", err))
	}

	arrivals := schedule(*teams, *scale)
	fmt.Printf("deadline crush: %d submissions over %s against %s\n", *teams, arrivals[len(arrivals)-1].Round(time.Second), *base)
	rec := &recorder{failed: map[string]int{}, start: time.Now()}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 400) // at most 400 teams in flight, like k6 maxVUs
	for i, at := range arrivals {
		time.Sleep(time.Until(rec.start.Add(at)))
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			size := sizes[i%len(sizes)]
			submit(client, *base, rows[i][0], rows[i][1], i, files[size], hashes[size], rec)
		})
		if (i+1)%250 == 0 {
			fmt.Printf("  %5d started at %6s\n", i+1, time.Since(rec.start).Round(time.Second))
		}
	}
	wg.Wait()
	report(rec, *teams, time.Since(rec.start), *base, *out, sizes)
}

// schedule returns when each of n arrivals happens, following the rising rate.
func schedule(n int, scale float64) []time.Duration {
	var times []time.Duration
	var acc, t, inStage float64
	rate := startRate
	const tick = 0.01
	for si := 0; len(times) < n && si < len(stages); {
		st := stages[si]
		dur := st.dur.Seconds()
		acc += (rate + (st.to-rate)*(inStage/dur)) / 60 * tick
		for acc >= 1 && len(times) < n {
			acc--
			times = append(times, time.Duration(t*scale*float64(time.Second)))
		}
		t += tick
		inStage += tick
		if inStage >= dur {
			rate, inStage = st.to, 0
			si++
		}
	}
	for len(times) < n { // rounding left a few: send them at the end
		times = append(times, time.Duration(t*scale*float64(time.Second)))
	}
	return times
}

// retryable is what a browser would retry: no answer, overload, a hiccup.
func retryable(status int) bool { return status == 0 || status == 429 || status >= 500 }

func backoff(attempt int) time.Duration {
	return time.Duration(math.Min(8, math.Pow(2, float64(attempt-1)))) * time.Second
}

func submit(c *http.Client, base, team, token string, i int, file []byte, sha string, rec *recorder) {
	// call sends one request, retrying like the browser client.
	call := func(step, method, url string, body []byte, headers map[string]string, ok func(int) bool) (int, []byte) {
		var code int
		var raw []byte
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			req, _ := http.NewRequest(method, url, bytes.NewReader(body))
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			t := time.Now()
			res, err := c.Do(req)
			s := sample{step: step, ms: float64(time.Since(t).Microseconds()) / 1000, at: t.Sub(rec.start), attempt: attempt}
			if err != nil {
				s.err = err.Error()
				code, raw = 0, nil
			} else {
				raw, _ = io.ReadAll(res.Body)
				res.Body.Close()
				code, s.status = res.StatusCode, res.StatusCode
			}
			rec.add(s)
			if ok(code) || !retryable(code) {
				return code, raw
			}
			time.Sleep(backoff(attempt))
		}
		return code, raw
	}
	is := func(codes ...int) func(int) bool {
		return func(c int) bool {
			for _, x := range codes {
				if c == x {
					return true
				}
			}
			return false
		}
	}
	api := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}

	body, _ := json.Marshal(map[string]any{"title": fmt.Sprintf("Load test team %d", i), "description": "", "links": []string{},
		"file": map[string]any{"name": "deck.pdf", "size": len(file), "sha256": sha}})
	h := map[string]string{"Idempotency-Key": "loadtest-" + team}
	maps.Copy(h, api)
	code, raw := call("create", "POST", base+"/api/teams/"+team+"/versions", body, h, is(200, 201))
	if code != 201 && code != 200 {
		rec.outcome(false, fmt.Sprintf("create %d", code))
		return
	}
	var created struct {
		Version struct{ ID string } `json:"version"`
	}
	_ = json.Unmarshal(raw, &created)
	id := created.Version.ID

	// Sign and PUT, re-signing when storage refuses (an expired URL answers 403).
	put := 0
	for round := 1; round <= 3 && put != 200; round++ {
		code, raw = call("sign", "POST", base+"/api/versions/"+id+"/parts/sign", []byte(`{"parts":[1]}`), api, is(200))
		if code != 200 {
			rec.outcome(false, fmt.Sprintf("sign %d", code))
			return
		}
		var signed struct {
			Parts []struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"parts"`
		}
		_ = json.Unmarshal(raw, &signed)
		p := signed.Parts[0]
		put, _ = call("storage_put", p.Method, p.URL, file, p.Headers, is(200))
	}
	if put != 200 {
		rec.outcome(false, fmt.Sprintf("put %d", put))
		return
	}

	code, raw = call("complete", "POST", base+"/api/versions/"+id+"/complete", []byte("{}"), api, is(200))
	var done struct {
		Version struct{ State string } `json:"version"`
	}
	_ = json.Unmarshal(raw, &done)
	if code != 200 || done.Version.State == "uploading" {
		rec.outcome(false, fmt.Sprintf("complete %d", code))
		return
	}
	rec.outcome(true, "")
}

type stepStats struct {
	Step     string         `json:"step"`
	Count    int            `json:"count"`
	Retries  int            `json:"retries"`
	Errors   int            `json:"errors"`
	Statuses map[string]int `json:"statuses"`
	P50MS    float64        `json:"p50_ms"`
	P95MS    float64        `json:"p95_ms"`
	P99MS    float64        `json:"p99_ms"`
	MaxMS    float64        `json:"max_ms"`
	MeanMS   float64        `json:"mean_ms"`
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func stats(name string, ss []sample) stepStats {
	st := stepStats{Step: name, Statuses: map[string]int{}}
	var ms []float64
	var sum float64
	for _, s := range ss {
		st.Count++
		if s.attempt > 1 {
			st.Retries++
		}
		if s.err != "" {
			st.Errors++
			st.Statuses["network error"]++
			continue
		}
		st.Statuses[fmt.Sprint(s.status)]++
		ms = append(ms, s.ms)
		sum += s.ms
	}
	sort.Float64s(ms)
	if len(ms) > 0 {
		st.P50MS, st.P95MS, st.P99MS, st.MaxMS, st.MeanMS = percentile(ms, .5), percentile(ms, .95), percentile(ms, .99), ms[len(ms)-1], sum/float64(len(ms))
	}
	return st
}

type minuteStats struct {
	Minute   int     `json:"minute"`
	Requests int     `json:"api_requests"`
	PerSec   float64 `json:"api_requests_per_second"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
}

func report(rec *recorder, teams int, elapsed time.Duration, base, outDir string, sizes []int) {
	by := map[string][]sample{}
	var apiSamples []sample
	perMinute := map[int][]float64{}
	for _, s := range rec.samples {
		by[s.step] = append(by[s.step], s)
		if s.step != "storage_put" {
			apiSamples = append(apiSamples, s)
			if s.err == "" {
				m := int(s.at / time.Minute)
				perMinute[m] = append(perMinute[m], s.ms)
			}
		}
	}
	var steps []stepStats
	for _, name := range []string{"create", "sign", "storage_put", "complete"} {
		steps = append(steps, stats(name, by[name]))
	}
	api := stats("all API calls", apiSamples)
	var minutes []minuteStats
	for m := 0; m <= int(elapsed/time.Minute); m++ {
		ms := perMinute[m]
		if len(ms) == 0 {
			continue
		}
		sort.Float64s(ms)
		minutes = append(minutes, minuteStats{Minute: m + 1, Requests: len(ms), PerSec: float64(len(ms)) / 60, P50MS: percentile(ms, .5), P95MS: percentile(ms, .95)})
	}
	result := map[string]any{
		"base_url": base, "finished_at": time.Now().UTC(), "elapsed_seconds": elapsed.Seconds(),
		"teams": teams, "completed": rec.completed, "failed": rec.failed, "file_sizes": sizes,
		"api": api, "steps": steps, "per_minute": minutes,
	}
	if err := os.MkdirAll(outDir, 0o755); err == nil {
		b, _ := json.MarshalIndent(result, "", "  ")
		_ = os.WriteFile(filepath.Join(outDir, "results.json"), b, 0o644)
	}

	fmt.Printf("\nfinished in %s: %d of %d submissions completed, failures %v\n", elapsed.Round(time.Second), rec.completed, teams, rec.failed)
	fmt.Printf("%-16s %6s %7s %7s %7s %7s %7s  %s\n", "step", "count", "retries", "p50", "p95", "p99", "max", "statuses")
	for _, s := range append(steps, api) {
		fmt.Printf("%-16s %6d %7d %6.0fms %6.0fms %6.0fms %6.0fms  %v\n", s.Step, s.Count, s.Retries, s.P50MS, s.P95MS, s.P99MS, s.MaxMS, s.Statuses)
	}
	fmt.Println("\nAPI latency by minute (client side):")
	for _, m := range minutes {
		fmt.Printf("  minute %2d: %5d requests (%5.1f/s)  p50 %5.0fms  p95 %5.0fms\n", m.Minute, m.Requests, m.PerSec, m.P50MS, m.P95MS)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "loadtest:", err)
	os.Exit(1)
}
