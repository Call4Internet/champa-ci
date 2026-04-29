package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"www.bamsoftware.com/git/champa.git/encapsulation"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	initPollDelay       = 300 * time.Millisecond
	maxPollDelay        = 5 * time.Second
	pollDelayMultiplier = 1.5

	pollTimeout = 25 * time.Second

	requestsPerSecondMax    = 8.0
	requestsPerSecondBurst  = 12.0
	requestsPerSecondRoI    = 0.10 // slower linear recovery to avoid quota spikes
	requestsPerSecondMulDec = 0.70

	// Quota back-off tuning.
	//
	// At concurrency=4 a single cache blip produces up to 4 quota errors at
	// once. The threshold is set conservatively so transient bursts don't fire
	// slow mode on their own.
	quotaErrThreshold = 20

	// Minimum gap between consecutive slow-mode activations.
	quotaCoolBetween = 45 * time.Second

	// Initial slow-mode duration; doubles on each activation up to quotaBackoffMax.
	quotaBackoffInit = 2 * time.Minute
	quotaBackoffMax  = 20 * time.Minute

	// Poll rate in slow mode: ~1 req/15s — enough for KCP keep-alives and
	// the two-round-trip Noise handshake.
	quotaSlowRate = 0.07

	// Hard-floor rate always honoured even in slow mode.
	minAlwaysRate = quotaSlowRate

	// Number of consecutive successful polls required before exiting slow mode
	// early. Without this guard a single lucky poll immediately cancels the
	// back-off, causing the quota/slow-mode cycle seen in the logs.
	slowModeExitSuccesses = 5
)

// QuotaManager tracks AMP cache quota errors and manages exponential back-off.
// Shared across session reconnects so state survives a session drop.
type QuotaManager struct {
	mu sync.Mutex

	consecErrors    int
	consecSuccesses int // counted only while in slow mode
	backoffDur      time.Duration
	slowUntil       time.Time
	lastActivate    time.Time
}

func NewQuotaManager() *QuotaManager {
	return &QuotaManager{backoffDur: quotaBackoffInit}
}

// RecordQuotaError counts one quota error and activates slow mode when the
// threshold is crossed.
func (q *QuotaManager) RecordQuotaError() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.consecErrors++
	q.consecSuccesses = 0 // reset success streak on any error

	if q.consecErrors < quotaErrThreshold {
		return false
	}
	if time.Since(q.lastActivate) < quotaCoolBetween {
		q.consecErrors = 0
		return false
	}
	// Already in slow mode: extend counter reset but do not stack timers.
	if time.Now().Before(q.slowUntil) {
		q.consecErrors = 0
		return false
	}

	q.consecErrors = 0
	q.slowUntil = time.Now().Add(q.backoffDur)
	q.lastActivate = time.Now()
	log.Printf("[quota] entering slow mode for %v (%.3f req/s)", q.backoffDur, quotaSlowRate)
	q.backoffDur *= 2
	if q.backoffDur > quotaBackoffMax {
		q.backoffDur = quotaBackoffMax
	}
	return true
}

// RecordSuccess resets the error counter.
//
// Slow mode is only exited early after slowModeExitSuccesses consecutive
// successful polls. This prevents a single lucky response from immediately
// cancelling a back-off period, which was causing the rapid slow↔normal
// cycling visible in the logs.
func (q *QuotaManager) RecordSuccess() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.consecErrors = 0

	if q.slowUntil.IsZero() || time.Now().After(q.slowUntil) {
		// Not in slow mode (or already expired by timeout) — nothing to do.
		return
	}

	// In slow mode: require several consecutive successes before exiting.
	q.consecSuccesses++
	if q.consecSuccesses >= slowModeExitSuccesses {
		log.Printf("[quota] %d consecutive successes — leaving slow mode early, resetting backoff",
			slowModeExitSuccesses)
		q.slowUntil = time.Time{}
		q.backoffDur = quotaBackoffInit
		q.consecSuccesses = 0
	}
}

// IsSlowMode reports whether slow mode is currently active.
// Automatically clears expired back-off periods.
func (q *QuotaManager) IsSlowMode() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.slowUntil.IsZero() {
		return false
	}
	if time.Now().After(q.slowUntil) {
		log.Printf("[quota] back-off elapsed — resuming normal rate")
		q.slowUntil = time.Time{}
		q.consecSuccesses = 0
		return false
	}
	return true
}

// ErrorCount returns the current consecutive error count.
func (q *QuotaManager) ErrorCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.consecErrors
}

// StatusSnapshot is a point-in-time view of connection health.
// Served as JSON at 127.0.0.1:7001/status.
type StatusSnapshot struct {
	SessionElapsedSec float64 `json:"session_elapsed_sec"`
	Conv              string  `json:"conv"`
	Connecting        bool    `json:"connecting"`
	IsSlowMode        bool    `json:"is_slow_mode"`
	QuotaErrors       int     `json:"quota_errors"`
	RatePerSec        float64 `json:"rate_per_sec"`
	BytesPerSec       float64 `json:"bytes_per_sec"`
	AvgRTTms          float64 `json:"avg_rtt_ms"`
	ErrorRate         float64 `json:"error_rate"`
	MaxPayload        int     `json:"max_payload"`
	Concurrency       int     `json:"concurrency"`
	CurrentCache      string  `json:"current_cache"`
}

// PollFunc sends payload p to the server and returns the decoded response body.
type PollFunc func(ctx context.Context, p []byte) (io.ReadCloser, error)

// PollingPacketConn implements net.PacketConn over HTTP polling.
type PollingPacketConn struct {
	remoteAddr net.Addr
	clientID   turbotunnel.ClientID
	ctx        context.Context
	cancel     context.CancelFunc
	adaptive   *AdaptiveController
	quota      *QuotaManager
	rotor      *CacheRotor
	poll       PollFunc

	// Atomic float64 storage for lock-free reads from the status endpoint.
	currentRateBits uint64

	*turbotunnel.QueuePacketConn
}

// NewPollingPacketConn creates a PollingPacketConn and starts its poll loop.
func NewPollingPacketConn(
	remoteAddr net.Addr,
	poll PollFunc,
	quota *QuotaManager,
	rotor *CacheRotor,
) *PollingPacketConn {
	clientID := turbotunnel.NewClientID()
	ctx, cancel := context.WithCancel(context.Background())
	c := &PollingPacketConn{
		remoteAddr:      remoteAddr,
		clientID:        clientID,
		ctx:             ctx,
		cancel:          cancel,
		adaptive:        NewAdaptiveController(),
		quota:           quota,
		rotor:           rotor,
		poll:            poll,
		QueuePacketConn: turbotunnel.NewQueuePacketConn(clientID, 0),
	}
	go func() {
		if err := c.pollLoop(); err != nil {
			log.Printf("pollLoop exited: %v", err)
		}
	}()
	return c
}

func (c *PollingPacketConn) Close() error {
	c.cancel()
	return c.QueuePacketConn.Close()
}

// CurrentRate returns the current poll rate (req/s) without locking.
func (c *PollingPacketConn) CurrentRate() float64 {
	return math.Float64frombits(atomic.LoadUint64(&c.currentRateBits))
}

func (c *PollingPacketConn) storeRate(r float64) {
	atomic.StoreUint64(&c.currentRateBits, math.Float64bits(r))
}

func (c *PollingPacketConn) pollLoop() error {
	rateLimit := NewRateLimiter(
		time.Now(),
		requestsPerSecondMax,
		requestsPerSecondBurst,
		requestsPerSecondRoI,
	)

	var (
		semMu  sync.Mutex
		semCap = 2
		semCur = 0
	)
	semAcquire := func() bool {
		semMu.Lock()
		defer semMu.Unlock()
		_, conc, _ := c.adaptive.GetParams()
		effectiveCap := conc
		if c.quota.IsSlowMode() {
			effectiveCap = 1
		}
		if conc != semCap {
			semCap = conc
		}
		if semCur >= effectiveCap {
			return false
		}
		semCur++
		return true
	}
	semRelease := func() {
		semMu.Lock()
		semCur--
		semMu.Unlock()
	}

	pollDelay := initPollDelay
	pollTimer := time.NewTimer(pollDelay)

	for {
		var p []byte
		unstash := c.QueuePacketConn.Unstash(c.remoteAddr)
		outgoing := c.QueuePacketConn.OutgoingQueue(c.remoteAddr)
		timerFired := false

		// Three-level priority: unstashed > fresh outgoing > idle timer.
		select {
		case <-c.ctx.Done():
			return nil
		default:
			select {
			case <-c.ctx.Done():
				return nil
			case p = <-unstash:
			default:
				select {
				case <-c.ctx.Done():
					return nil
				case p = <-unstash:
				case p = <-outgoing:
				default:
					select {
					case <-c.ctx.Done():
						return nil
					case p = <-unstash:
					case p = <-outgoing:
					case <-pollTimer.C:
						timerFired = true
					}
				}
			}
		}

		if timerFired {
			pollDelay = time.Duration(float64(pollDelay) * pollDelayMultiplier)
			if pollDelay > maxPollDelay {
				pollDelay = maxPollDelay
			}
		} else {
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollDelay = initPollDelay
		}
		pollTimer.Reset(pollDelay)

		// Build outgoing payload.
		maxPayload, _, _ := c.adaptive.GetParams()
		var payload bytes.Buffer
		payload.Write(c.clientID[:])
		first := true
		for len(p) > 0 && (first || payload.Len()+len(p) <= maxPayload) {
			first = false
			encapsulation.WriteData(&payload, p)
			select {
			case p = <-outgoing:
			default:
				p = nil
			}
		}
		if len(p) > 0 {
			c.QueuePacketConn.Stash(p, c.remoteAddr)
		}

		// Rate control: slow mode overrides adaptive target; hard floor always applies.
		now := time.Now()
		if c.quota.IsSlowMode() {
			if rateLimit.GetRate() > quotaSlowRate {
				rateLimit.SetRate(quotaSlowRate)
			}
		} else {
			_, _, targetRate := c.adaptive.GetParams()
			if rateLimit.GetRate() < targetRate {
				rateLimit.SetRate(targetRate)
			}
		}
		if rateLimit.GetRate() < minAlwaysRate {
			rateLimit.SetRate(minAlwaysRate)
		}
		c.storeRate(rateLimit.GetRate())

		if limited, _ := rateLimit.IsLimited(now); limited {
			continue
		}
		if !semAcquire() {
			continue
		}
		rateLimit.Take(now, 1.0)

		snap := make([]byte, payload.Len())
		copy(snap, payload.Bytes())

		go func() {
			defer semRelease()

			start := time.Now()
			ctx, cancel := context.WithTimeout(c.ctx, pollTimeout)
			defer cancel()

			body, err := c.poll(ctx, snap)
			if err != nil {
				if errors.Is(err, ErrCacheQuota) {
					c.quota.RecordQuotaError()
					c.rotor.RecordQuotaError()
					return
				}
				log.Printf("poll error (%.2f req/s): %v", rateLimit.GetRate(), err)
				rateLimit.MultiplicativeDecrease(time.Now(), requestsPerSecondMulDec)
				c.adaptive.RecordError()
				return
			}
			defer body.Close()

			n, err := c.processIncoming(body)
			rtt := time.Since(start)
			if err != nil {
				log.Printf("processIncoming: %v", err)
				c.adaptive.RecordError()
			} else {
				c.quota.RecordSuccess()
				c.rotor.RecordSuccess()
				c.adaptive.RecordSuccess(int64(n), rtt)
			}
		}()
	}
}

func (c *PollingPacketConn) processIncoming(body io.Reader) (int, error) {
	const maxResponseBody = 1 * 1024 * 1024
	lr := io.LimitReader(body, maxResponseBody)
	total := 0
	for {
		p, err := encapsulation.ReadData(lr)
		if err != nil {
			if err == io.EOF && lr.(*io.LimitedReader).N == 0 {
				err = errors.New("response body exceeded 1 MB limit")
			} else if err == io.EOF {
				err = nil
			}
			return total, err
		}
		total += len(p)
		c.QueuePacketConn.QueueIncoming(p, c.remoteAddr)
	}
}
