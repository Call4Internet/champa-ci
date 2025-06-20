package main

import (
	"time"
)

// Leaky bucket rate limiter. Lets you do rate units per second, in a burst of
// up to max.
type RateLimiter struct {
	rate       float64
	max        float64
	cur        float64 // < 0 means limited, >= 0 means free to act
	lastUpdate time.Time
}

// NewRateLimiter creates a new RateLimiter with the given parameters.
func NewRateLimiter(now time.Time, rate, max float64) RateLimiter {
	return RateLimiter{
		rate:       rate,
		max:        max,
		cur:        0.0,
		lastUpdate: now,
	}
}

// replenish refills the bucket for the amount of time that has passed since the
// last update at the current rate, up to max.
func (rl *RateLimiter) update(now time.Time) {
	if now.Before(rl.lastUpdate) {
		return
	}
	rl.cur = rl.cur + rl.rate*now.Sub(rl.lastUpdate).Seconds()
	if rl.cur > rl.max {
		rl.cur = rl.max
	}
	rl.lastUpdate = now
}

// IsLimited returns two values: a bool indicating whether the limiter is
// currently limiting, and a time.Duration indicating an amount of time after
// which it will be free to act again.
func (rl *RateLimiter) IsLimited(now time.Time) (bool, time.Duration) {
	rl.replenish(now)
	if rl.cur < 0.0 {
		return true, time.Duration(-rl.cur / rl.rate * 1e9)
	} else {
		return false, 0
	}
}

// Take removes an amount of capacity from the bucket. If this causes the
// capacity to go negative, the limiter will start limiting.
func (rl *RateLimiter) Take(now time.Time, amount float64) {
	rl.replenish(now)
	rl.cur -= amount
}
