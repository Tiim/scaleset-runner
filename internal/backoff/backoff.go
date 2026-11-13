// Package backoff implements the growing delays every retry of the Controller
// uses (NFR-013): 1 second, doubled with each failed attempt, varied randomly
// by up to 20%, and capped at 5 minutes.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	Initial = time.Second
	Max     = 5 * time.Minute
	Jitter  = 0.2
)

// Delay returns the delay before retry number attempt (0 for the first retry).
func Delay(attempt int) time.Duration {
	d := Initial
	for i := 0; i < attempt && d < Max; i++ {
		d *= 2
	}
	d = min(d, Max)
	// Vary by up to ±20%, then cap again so the delay never exceeds Max.
	f := 1 + Jitter*(2*rand.Float64()-1)
	return min(time.Duration(float64(d)*f), Max)
}

// Sleep waits for d or until ctx is done, returning ctx.Err() in that case.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
