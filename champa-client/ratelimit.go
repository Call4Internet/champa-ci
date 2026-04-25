package main

import (
	"time"
)

// RateLimiter is a leaky-bucket rate limiter.
//
// It allows up to `rate` operations per second, burst-capped at `max`.
// The rate increases linearly at `rateRateOfIncrease` units per second,
// modelling a connection that slowly recovers after congestion.
//
// Negative `cur` means the bucket is overdrawn (limited).
// Zero or positive `cur` means operations are allowed.
type RateLimiter struct {
	rate               float64
	max                float64
	rateRateOfIncrease float64
	cur                float64
	lastUpdate         time.Time
}

// NewRateLimiter creates a new RateLimiter with the given parameters.
func NewRateLimiter(now time.Time, rate, max, rateRateOfIncrease float64) RateLimiter {
	return RateLimiter{
		rate:               rate,
		max:                max,
		rateRateOfIncrease: rateRateOfIncrease,
		// Seed with a small positive value so the very first operation
		// is not gated.
		cur:        1.0,
		lastUpdate: now,
	}
}

// update refills the bucket for elapsed time at the current rate, up to max,
// and increases the rate by rateRateOfIncrease per elapsed second.
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
		// Never let the rate exceed the burst cap.
		rl.rate = rl.max
	}
}

// IsLimited returns whether the limiter is currently limiting, and a duration
// estimate of how long until it will allow an operation.
func (rl *RateLimiter) IsLimited(now time.Time) (bool, time.Duration) {
	rl.update(now)
	if rl.cur < 0.0 {
		return true, time.Duration(-rl.cur / rl.rate * float64(time.Second))
	}
	return false, 0
}

// Take removes `amount` capacity from the bucket.
// If this drives cur below zero, subsequent calls to IsLimited will return true.
func (rl *RateLimiter) Take(now time.Time, amount float64) {
	rl.update(now)
	rl.cur -= amount
}

// MultiplicativeDecrease multiplies the current rate by factor (0 < factor < 1),
// and resets the bucket to 0 so the next operation is not penalised twice.
func (rl *RateLimiter) MultiplicativeDecrease(now time.Time, factor float64) {
	rl.update(now)
	rl.rate *= factor
	if rl.rate < adpMinRate {
		rl.rate = adpMinRate
	}
	rl.cur = 0.0
}

// GetRate returns the current rate (operations per second).
func (rl *RateLimiter) GetRate() float64 {
	return rl.rate
}

// SetRate sets the target rate to r (clamped to [adpMinRate, adpMaxRate]).
// The bucket level is preserved so an increase in rate does not cause a burst.
func (rl *RateLimiter) SetRate(r float64) {
	r = clampFloat(r, adpMinRate, adpMaxRate)
	rl.rate = r
	if rl.max < r {
		rl.max = r * 2 // keep burst headroom
	}
}
