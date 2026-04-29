package main

import (
	"log"
	"sync"
	"time"
)

const (
	adpMinPayload = 1024
	adpMaxPayload = 7000

	adpMinConcurrency = 1
	// Reduced from 8 to 4. At concurrency=8, in-flight polls arrive at the
	// server heavily out-of-order. The Noise layer uses a bounded replay
	// window; when more than window-size packets are simultaneously in flight
	// the server rejects them with "nonce is already used or out of window".
	// Keeping concurrency <= 4 dramatically reduces out-of-order delivery.
	adpMaxConcurrency = 4

	adpMinRate = 2.0
	// Reduced from 12 to 8 to match the lower concurrency ceiling and avoid
	// overwhelming the AMP cache quota early in a session.
	adpMaxRate = 8.0

	adpWindowDuration = 8 * time.Second
)

// AdaptiveController monitors network conditions and tunes three parameters
// every adpWindowDuration:
//
//   - MaxPayload   -- bytes per outgoing poll body
//   - Concurrency  -- max parallel in-flight polls
//   - TargetRate   -- desired polls/sec (fed to RateLimiter)
//
// Decision table evaluated once per window:
//
//	error rate > 30%          aggressive back-off on all three
//	error rate > 10%          gentle back-off on payload and rate
//	low throughput + clean    probe upward on all three
//	medium throughput + clean nudge payload and rate up
//	otherwise                 hold steady
type AdaptiveController struct {
	mu sync.Mutex

	MaxPayload  int
	Concurrency int
	TargetRate  float64

	winStart    time.Time
	winBytes    int64
	winReqs     int64
	winErrors   int64
	winRTTSum   float64
	winRTTCount int64

	// Updated each window; safe to read for logging / status snapshots.
	BytesPerSec float64
	AvgRTTms    float64
	ErrorRate   float64
}

// NewAdaptiveController returns a controller with conservative starting values.
// Concurrency starts at 2 and rate at 4.0 to avoid triggering the Noise nonce
// window overflow before the connection is fully warmed up.
func NewAdaptiveController() *AdaptiveController {
	return &AdaptiveController{
		MaxPayload:  3000,
		Concurrency: 2,
		TargetRate:  4.0,
		winStart:    time.Now(),
	}
}

// RecordSuccess records a successful poll.
func (a *AdaptiveController) RecordSuccess(bytes int64, rtt time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.winBytes += bytes
	a.winReqs++
	a.winRTTSum += float64(rtt.Milliseconds())
	a.winRTTCount++
	a.maybeAdapt()
}

// RecordError records a failed poll (non-quota errors only).
func (a *AdaptiveController) RecordError() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.winErrors++
	a.maybeAdapt()
}

// GetParams returns a snapshot of current parameters. Safe for concurrent use.
func (a *AdaptiveController) GetParams() (maxPayload, concurrency int, targetRate float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.MaxPayload, a.Concurrency, a.TargetRate
}

// GetStats returns smoothed stats for status snapshots.
func (a *AdaptiveController) GetStats() (bytesPerSec, avgRTTms, errorRate float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.BytesPerSec, a.AvgRTTms, a.ErrorRate
}

// maybeAdapt evaluates the window and adjusts parameters when it has elapsed.
// Must be called with a.mu held.
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
		// High error rate: back off aggressively on all three parameters.
		a.MaxPayload = clampInt(int(float64(a.MaxPayload)*0.75), adpMinPayload, adpMaxPayload)
		a.Concurrency = clampInt(a.Concurrency-1, adpMinConcurrency, adpMaxConcurrency)
		a.TargetRate = clampFloat(a.TargetRate*0.70, adpMinRate, adpMaxRate)

	case a.ErrorRate > 0.10:
		// Moderate error rate: gentle back-off on payload and rate.
		a.MaxPayload = clampInt(int(float64(a.MaxPayload)*0.90), adpMinPayload, adpMaxPayload)
		a.TargetRate = clampFloat(a.TargetRate*0.85, adpMinRate, adpMaxRate)

	case a.BytesPerSec < 30*1024 && a.ErrorRate < 0.05:
		// Low throughput on a clean connection: probe all parameters upward.
		a.MaxPayload = clampInt(a.MaxPayload+700, adpMinPayload, adpMaxPayload)
		a.Concurrency = clampInt(a.Concurrency+1, adpMinConcurrency, adpMaxConcurrency)
		a.TargetRate = clampFloat(a.TargetRate+0.5, adpMinRate, adpMaxRate)

	case a.BytesPerSec < 100*1024 && a.ErrorRate < 0.05:
		// Medium throughput on a clean connection: nudge payload and rate.
		a.MaxPayload = clampInt(a.MaxPayload+300, adpMinPayload, adpMaxPayload)
		a.TargetRate = clampFloat(a.TargetRate+0.25, adpMinRate, adpMaxRate)

	default:
		// Healthy throughput: hold steady.
	}

	log.Printf(
		"[adaptive] %.1f KB/s  rtt=%.0fms  err=%.0f%%  payload:%d->%d  conc:%d->%d  rate:%.1f->%.1f/s",
		a.BytesPerSec/1024, a.AvgRTTms, a.ErrorRate*100,
		prevPayload, a.MaxPayload,
		prevConc, a.Concurrency,
		prevRate, a.TargetRate,
	)

	// Reset accumulators for the next window.
	a.winStart = time.Now()
	a.winBytes, a.winReqs, a.winErrors = 0, 0, 0
	a.winRTTSum, a.winRTTCount = 0, 0
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
