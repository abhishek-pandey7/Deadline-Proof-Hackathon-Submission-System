package main

import (
	"testing"
	"time"
)

// The schedule must put 2,000 submissions inside 10 minutes, densest at the end.
func TestScheduleIsTenMinutesAndClimbs(t *testing.T) {
	at := schedule(2000, 1)
	if len(at) != 2000 {
		t.Fatalf("%d arrivals, want 2000", len(at))
	}
	last := at[len(at)-1]
	if last < 9*time.Minute+30*time.Second || last > 10*time.Minute+5*time.Second {
		t.Fatalf("last arrival at %v, want about 10 minutes", last)
	}
	firstMinute, lastMinute := 0, 0
	for _, a := range at {
		if a < time.Minute {
			firstMinute++
		}
		if a >= 9*time.Minute {
			lastMinute++
		}
	}
	if firstMinute < 50 || firstMinute > 80 || lastMinute < 380 {
		t.Fatalf("first minute %d arrivals (want ~60), last minute %d (want ~450)", firstMinute, lastMinute)
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Fatal("arrivals out of order")
		}
	}
}
