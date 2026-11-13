package backoff

import (
	"testing"
	"time"
)

func TestDelayGrowsAndIsCapped(t *testing.T) {
	for attempt, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		for range 100 {
			d := Delay(attempt)
			lo := time.Duration(float64(base) * 0.8)
			hi := time.Duration(float64(base) * 1.2)
			if d < lo || d > hi {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, lo, hi)
			}
		}
	}
	for range 100 {
		if d := Delay(50); d > Max || d < time.Duration(float64(Max)*0.8) {
			t.Fatalf("capped delay %v outside [0.8*Max, Max]", d)
		}
	}
}
