package steam

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

const (
	backoffBase = 500 * time.Millisecond
	backoffMax  = 10 * time.Second
)

// sleepBackoff espera o atraso da n-ésima tentativa (exp. com jitter), abortando se o ctx cancelar.
func sleepBackoff(ctx context.Context, attempt int) error {
	d := backoffFor(attempt)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoffFor(attempt int) time.Duration {
	exp := math.Pow(2, float64(attempt-1))
	d := float64(backoffBase) * exp
	if d > float64(backoffMax) {
		d = float64(backoffMax)
	}
	// jitter ±20%
	d *= 1 + 0.2*(2*rand.Float64()-1) //nolint:gosec // jitter não-criptográfico
	return time.Duration(d)
}
