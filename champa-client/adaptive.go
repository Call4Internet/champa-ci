package main

import (
	"log"
	"sync"
	"time"
)

const (
	// Payload size bounds
	adpMinPayload = 1024
	adpMaxPayload = 7000

	// Concurrency bounds (parallel in-flight polls)
	adpMinConcurrency = 2
	adpMaxConcurrency = 8

	// Rate bounds (requests/sec)
	adpMinRate = 2.0
	adpMaxRate = 12.0

	// Measurement window
	adpWindowDuration = 8 * time.Second
)

// AdaptiveController monitors network conditions and dynamically tunes:
//   - MaxPayload   : bytes sent per poll request
//   - Concurrency  : maximum parallel in-flight polls
//   - TargetRate   : desired polls per second (fed back to RateLimiter)
//
// Algorithm:
//   - Every adpWindowDuration seconds it evaluates the error rate and
//     throughput observed during that window.
//   - High error rate  → reduce all parameters (back-off).
//   - Low throughput + low errors → increase parameters (probe).
//   - Otherwise        → hold steady (stable).
type AdaptiveController struct {
	mu sync.Mutex

	// Current tunable parameters (read by pollLoop)
	MaxPayload  int
	Concurrency int
	TargetRate  float64

	// Measurement window accumulators
	winStart    time.Time
	winBytes    int64 // bytes received in responses
	winReqs     int64 // successful polls
	winErrors   int64 // failed polls
	winRTTSum   float64
	winRTTCount int64

	// Smoothed stats (used for logging / external inspection)
	BytesPerSec float64
	AvgRTTms    float64
	ErrorRate   float64
}

// NewAdaptiveController returns a controller with conservative starting values.
// Parameters are probed upward after the first measurement window.
func NewAdaptiveController() *AdaptiveController {
	return &AdaptiveController{
		MaxPayload:  3000,
		Concurrency: 3,
		TargetRate:  6.0,
		winStart:    time.Now(),
	}
}

// RecordSuccess is called after each successful poll response.
// bytes is the number of payload bytes received; rtt is the round-trip time.
func (a *AdaptiveController) RecordSuccess(bytes int64, rtt time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.winBytes += bytes
	a.winReqs++
	a.winRTTSum += float64(rtt.Milliseconds())
	a.winRTTCount++
	a.maybeAdapt()
}

// RecordError is called after each failed poll.
func (a *AdaptiveController) RecordError() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.winErrors++
	a.maybeAdapt()
}

// GetParams returns a snapshot of the current adaptive parameters.
// Safe to call without holding the lock (uses atomic-safe copy under lock).
func (a *AdaptiveController) GetParams() (maxPayload, concurrency int, targetRate float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.MaxPayload, a.Concurrency, a.TargetRate
}

// maybeAdapt evaluates the current window and adjusts parameters if the window
// has elapsed. Must be called with a.mu held.
func (a *AdaptiveController) maybeAdapt() {
	elapsed := time.Since(a.winStart).Seconds()
	if elapsed < adpWindowDuration.Seconds() {
		return
	}

	total := a.winReqs + a.winErrors
	if total > 0 {
		a.BytesPerSec = float64(a.winBytes) / elapsed
		a.ErrorRate = float64(a.winErrors) / float64(total)
	}
	if a.winRTTCount > 0 {
		a.AvgRTTms = a.winRTTSum / float64(a.winRTTCount)
	}

	prevPayload := a.MaxPayload
	prevConc := a.Concurrency
	prevRate := a.TargetRate

	switch {
	case a.ErrorRate > 0.30:
		// High errors: back off aggressively
		a.MaxPayload = clampInt(int(float64(a.MaxPayload)*0.75), adpMinPayload, adpMaxPayload)
		a.Concurrency = clampInt(a.Concurrency-1, adpMinConcurrency, adpMaxConcurrency)
		a.TargetRate = clampFloat(a.TargetRate*0.7, adpMinRate, adpMaxRate)

	case a.ErrorRate > 0.10:
		// Moderate errors: gentle back-off
		a.MaxPayload = clampInt(int(float64(a.MaxPayload)*0.90), adpMinPayload, adpMaxPayload)
		a.TargetRate = clampFloat(a.TargetRate*0.85, adpMinRate, adpMaxRate)

	case a.BytesPerSec < 30*1024 && a.ErrorRate < 0.05:
		// Low throughput, clean connection: probe upward
		a.MaxPayload = clampInt(a.MaxPayload+700, adpMinPayload, adpMaxPayload)
		a.Concurrency = clampInt(a.Concurrency+1, adpMinConcurrency, adpMaxConcurrency)
		a.TargetRate = clampFloat(a.TargetRate+1.0, adpMinRate, adpMaxRate)

	case a.BytesPerSec < 100*1024 && a.ErrorRate < 0.05:
		// Medium throughput, clean: nudge upward
		a.MaxPayload = clampInt(a.MaxPayload+300, adpMinPayload, adpMaxPayload)
		a.TargetRate = clampFloat(a.TargetRate+0.5, adpMinRate, adpMaxRate)

	default:
		// Healthy throughput: hold steady
	}

	log.Printf(
		"[adaptive] %.1f KB/s  rtt=%.0fms  err=%.0f%%  payload:%d→%d  conc:%d→%d  rate:%.1f→%.1f/s",
		a.BytesPerSec/1024, a.AvgRTTms, a.ErrorRate*100,
		prevPayload, a.MaxPayload,
		prevConc, a.Concurrency,
		prevRate, a.TargetRate,
	)

	// Reset window
	a.winStart = time.Now()
	a.winBytes = 0
	a.winReqs = 0
	a.winErrors = 0
	a.winRTTSum = 0
	a.winRTTCount = 0
}

// clampInt returns v clamped to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampFloat returns v clamped to [lo, hi].
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
