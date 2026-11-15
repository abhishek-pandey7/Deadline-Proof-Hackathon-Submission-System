// Command loadtest runs the "deadline crush": 2,000 teams submit within 10
// minutes, the arrival rate climbing toward the end (60 -> 150 per minute over
// 5 minutes, 150 -> 300 over 3, 300 -> 500 over 2). Each team runs the real
// flow: create a version, sign its part, PUT the bytes straight to storage,
// complete. It is the same scenario as loadtest/submit.js for k6.
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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type stage struct {
	to  float64 // arrivals per minute at the end of the stage
	dur time.Duration
}

var stages = []stage{{150, 5 * time.Minute}, {300, 3 * time.Minute}, {500, 2 * time.Minute}}

const startRate = 60.0

// File sizes cycle through these: mostly small decks, some bigger.
var sizes = []int{256 << 10, 256 << 10, 256 << 10, 512 << 10, 1 << 20, 2 << 20}

type sample struct {
	step   string
	status int
	ms     float64
	err    string
}

type recorder struct {
	mu        sync.Mutex
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
	out := flag.String("out", "loadtest/results", "folder for results.json")
	flag.Parse()

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
	rec := &recorder{failed: map[string]int{}}

	// Wake a sleeping free instance before the clock starts.
	if res, err := client.Get(*base + "/healthz"); err != nil || res.StatusCode != 200 {
		fail(fmt.Errorf("server not healthy: %v", err))
	}

	arrivals := schedule(*teams, *scale)
	fmt.Printf("deadline crush: %d submissions over %s against %s\n", *teams, arrivals[len(arrivals)-1].Round(time.Second), *base)
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 400) // at most 400 teams in flight, like k6 maxVUs
	for i, at := range arrivals {
		time.Sleep(time.Until(start.Add(at)))
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			team, token := rows[i][0], rows[i][1]
			size := sizes[i%len(sizes)]
			submit(client, *base, team, token, i, files[size], hashes[size], rec)
		})
		if (i+1)%250 == 0 {
			fmt.Printf("  %5d started at %6s\n", i+1, time.Since(start).Round(time.Second))
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	report(rec, *teams, elapsed, *base, *out)
}

// schedule returns when each of n arrivals happens, following the rising rate.
func schedule(n int, scale float64) []time.Duration {
	var times []time.Duration
	var acc, t float64 // acc: arrivals owed so far; t: seconds
	rate := startRate
	const tick = 0.01
	var elapsedInStage float64
	si := 0
	for len(times) < n && si < len(stages) {
		st := stages[si]
		dur := st.dur.Seconds()
		r := rate + (st.to-rate)*(elapsedInStage/dur)
		acc += r / 60 * tick
		for acc >= 1 && len(times) < n {
			acc--
			times = append(times, time.Duration(t*scale*float64(time.Second)))
		}
		t += tick
		elapsedInStage += tick
		if elapsedInStage >= dur {
			rate, elapsedInStage = st.to, 0
			si++
		}
	}
	for len(times) < n { // rounding left a few: send them at the end
		times = append(times, time.Duration(t*scale*float64(time.Second)))
	}
	return times
}

func submit(c *http.Client, base, team, token string, i int, file []byte, sha string, rec *recorder) {
	call := func(step, method, url string, body []byte, headers map[string]string) (int, []byte) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		t := time.Now()
		res, err := c.Do(req)
		ms := float64(time.Since(t).Microseconds()) / 1000
		if err != nil {
			rec.add(sample{step: step, ms: ms, err: err.Error()})
			return 0, nil
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		rec.add(sample{step: step, status: res.StatusCode, ms: ms})
		return res.StatusCode, b
	}
	api := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}

	body, _ := json.Marshal(map[string]any{"title": fmt.Sprintf("Load test team %d", i), "description": "", "links": []string{},
		"file": map[string]any{"name": "deck.pdf", "size": len(file), "sha256": sha}})
	h := map[string]string{"Idempotency-Key": "loadtest-" + team}
	for k, v := range api {
		h[k] = v
	}
	code, raw := call("create", "POST", base+"/api/teams/"+team+"/versions", body, h)
	if code != 201 && code != 200 {
		rec.outcome(false, fmt.Sprintf("create %d", code))
		return
	}
	var created struct {
		Version struct{ ID string } `json:"version"`
	}
	_ = json.Unmarshal(raw, &created)
	id := created.Version.ID

	code, raw = call("sign", "POST", base+"/api/versions/"+id+"/parts/sign", []byte(`{"parts":[1]}`), api)
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
	if code, _ = call("storage_put", p.Method, p.URL, file, p.Headers); code != 200 {
		rec.outcome(false, fmt.Sprintf("put %d", code))
		return
	}

	code, raw = call("complete", "POST", base+"/api/versions/"+id+"/complete", []byte("{}"), api)
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

func report(rec *recorder, teams int, elapsed time.Duration, base, outDir string) {
	by := map[string][]sample{}
	var apiSamples []sample
	for _, s := range rec.samples {
		by[s.step] = append(by[s.step], s)
		if s.step != "storage_put" {
			apiSamples = append(apiSamples, s)
		}
	}
	var steps []stepStats
	for _, name := range []string{"create", "sign", "storage_put", "complete"} {
		steps = append(steps, stats(name, by[name]))
	}
	api := stats("all API calls", apiSamples)
	result := map[string]any{
		"base_url": base, "finished_at": time.Now().UTC(), "elapsed_seconds": elapsed.Seconds(),
		"teams": teams, "completed": rec.completed, "failed": rec.failed,
		"api": api, "steps": steps,
	}
	if err := os.MkdirAll(outDir, 0o755); err == nil {
		b, _ := json.MarshalIndent(result, "", "  ")
		_ = os.WriteFile(filepath.Join(outDir, "results.json"), b, 0o644)
	}

	fmt.Printf("\nfinished in %s: %d of %d submissions completed, failures %v\n", elapsed.Round(time.Second), rec.completed, teams, rec.failed)
	fmt.Printf("%-16s %6s %7s %7s %7s %7s  %s\n", "step", "count", "p50", "p95", "p99", "max", "statuses")
	for _, s := range append(steps, api) {
		fmt.Printf("%-16s %6d %6.0fms %6.0fms %6.0fms %6.0fms  %v\n", s.Step, s.Count, s.P50MS, s.P95MS, s.P99MS, s.MaxMS, s.Statuses)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "loadtest:", err)
	os.Exit(1)
}
