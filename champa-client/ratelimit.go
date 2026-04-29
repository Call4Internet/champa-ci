package main

import (
	"time"
)

// RateLimiter is a token-bucket rate limiter with a linearly recovering rate.
//
// Invariant:
//
//	cur >= 0   operations are allowed
//	cur <  0   rate-limited; IsLimited returns true
//
// The refill rate increases by rateRateOfIncrease tokens per second, modelling
// a connection recovering from congestion (AIMD: slow linear increase, fast
// multiplicative decrease).
type RateLimiter struct {
	rate               float64
	max                float64
	rateRateOfIncrease float64
	cur                float64
	lastUpdate         time.Time
}

// NewRateLimiter returns a RateLimiter seeded with cur=1 so the very first
// operation is never gated.
func NewRateLimiter(now time.Time, rate, max, rateRateOfIncrease float64) RateLimiter {
	return RateLimiter{
		rate:               rate,
		max:                max,
		rateRateOfIncrease: rateRateOfIncrease,
		cur:                1.0,
		lastUpdate:         now,
	}
}

// update refills the bucket for elapsed time and increases the rate.
// Must be called before every public method.
func (rl *RateLimiter) update(now time.Time) {
	if now.Before(rl.lastUpdate) {
		return
	}
	elapsed := now.Sub(rl.lastUpdate).Seconds()
	rl.lastUpdate = now

	rl.cur += rl.rate * elapsed
	if rl.cur > rl.max {
		rl.cur = rl.max
	}

	rl.rate += rl.rateRateOfIncrease * elapsed
	if rl.rate > rl.max {
		rl.rate = rl.max
	}
}

// IsLimited reports whether the limiter is currently throttling and returns an
// estimate of how long until the next operation will be allowed.
func (rl *RateLimiter) IsLimited(now time.Time) (bool, time.Duration) {
	rl.update(now)
	if rl.cur < 0.0 {
		wait := time.Duration(-rl.cur / rl.rate * float64(time.Second))
		return true, wait
	}
	return false, 0
}

// Take removes amount tokens from the bucket. A subsequent call to IsLimited
// will return true until the bucket has refilled sufficiently.
func (rl *RateLimiter) Take(now time.Time, amount float64) {
	rl.update(now)
	rl.cur -= amount
}

// MultiplicativeDecrease multiplies the current rate by factor (0 < factor < 1)
// and resets cur to 0 so the caller is not penalised twice.
// Used on poll errors to implement TCP-style congestion control.
func (rl *RateLimiter) MultiplicativeDecrease(now time.Time, factor float64) {
	rl.update(now)
	rl.rate *= factor
	if rl.rate < adpMinRate {
		rl.rate = adpMinRate
	}
	rl.cur = 0.0
}

// GetRate returns the current refill rate (operations per second).
func (rl *RateLimiter) GetRate() float64 { return rl.rate }

// SetRate sets the refill rate to r, clamped to [adpMinRate, adpMaxRate].
// The bucket level is preserved to avoid an artificial burst on rate increase.
func (rl *RateLimiter) SetRate(r float64) {
	r = clampFloat(r, adpMinRate, adpMaxRate)
	rl.rate = r
	if rl.max < r {
		rl.max = r * 2 // keep burst headroom
	}
}
