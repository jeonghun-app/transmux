package channel

import (
	"math"
	"math/rand"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

// backoff produces the reconnect delay sequence.
//
// Reset is gated on sustained progress, not on a successful TCP connect. A
// camera that accepts the RTSP session and then delivers nothing would
// otherwise reset the backoff on every attempt and reconnect in a tight
// loop.
type backoff struct {
	cfg     config.ReconnectConfig
	attempt int
	rng     *rand.Rand
}

func newBackoff(cfg config.ReconnectConfig, seed int64) *backoff {
	return &backoff{cfg: cfg, rng: rand.New(rand.NewSource(seed))}
}

// Next returns the delay before the next attempt and advances the sequence.
func (b *backoff) Next() time.Duration {
	base := float64(b.cfg.Base.Duration)
	capped := math.Min(base*math.Pow(b.cfg.Factor, float64(b.attempt)), float64(b.cfg.Max.Duration))
	b.attempt++
	// Full jitter: uniform in [0, capped]. A floor of base/2 keeps the very
	// first retry from being effectively immediate.
	floor := base / 2
	d := floor + b.rng.Float64()*(capped-floor)
	if d < floor {
		d = floor
	}
	return time.Duration(d)
}

// Attempt reports how many consecutive failures have occurred.
func (b *backoff) Attempt() int { return b.attempt }

// Reset clears the sequence. Callers must only do this after the channel has
// produced segments for at least ReconnectConfig.ResetAfter.
func (b *backoff) Reset() { b.attempt = 0 }
