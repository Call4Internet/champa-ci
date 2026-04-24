package main

import (
	"time"
)

// Leaky bucket rate limiter with AIMD (Additive Increase Multiplicative Decrease)
// بهتر‌سازی: سرعت بیشتر و تطبیقی‌تر
type RateLimiter struct {
	rate               float64
	max                float64
	rateRateOfIncrease float64
	cur                float64
	lastUpdate         time.Time
}

func NewRateLimiter(now time.Time, rate, max, rateRateOfIncrease float64) RateLimiter {
	return RateLimiter{
		rate:               rate,
		max:                max,
		rateRateOfIncrease: rateRateOfIncrease,
		lastUpdate:         now,
	}
}

func (rl *RateLimiter) update(now time.Time) {
	if now.Before(rl.lastUpdate) {
		return
	}
	elapsed := now.Sub(rl.lastUpdate).Seconds()
	rl.lastUpdate = now
	
	// Replenish bucket
	rl.cur = rl.cur + rl.rate*elapsed
	if rl.cur > rl.max {
		rl.cur = rl.max
	}
	
	// Increase rate linearly
	rl.rate += rl.rateRateOfIncrease * elapsed
}

func (rl *RateLimiter) IsLimited(now time.Time) (bool, time.Duration) {
	rl.update(now)
	if rl.cur < 0.0 {
		return true, time.Duration(-rl.cur / rl.rate * 1e9)
	}
	return false, 0
}

func (rl *RateLimiter) Take(now time.Time, amount float64) {
	rl.update(now)
	rl.cur -= amount
}

func (rl *RateLimiter) MultiplicativeDecrease(now time.Time, factor float64) {
	rl.update(now)
	rl.rate *= factor
	rl.cur = 0.0
}
