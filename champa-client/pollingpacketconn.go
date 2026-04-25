package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"www.bamsoftware.com/git/champa.git/encapsulation"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	// pollLoop sends an empty keep-alive poll after this much idle time.
	// Starts at initPollDelay and backs off to maxPollDelay when idle.
	initPollDelay       = 300 * time.Millisecond // was 1s — faster initial reaction
	maxPollDelay        = 5 * time.Second        // was 10s
	pollDelayMultiplier = 1.5                    // was 2.0 — gentler back-off

	// Timeout for a complete request–response cycle including body read.
	pollTimeout = 25 * time.Second // was 30s

	// Initial / maximum request rate.  The AdaptiveController may raise or
	// lower TargetRate at runtime; these constants only seed the RateLimiter
	// that is created once at startup.
	requestsPerSecondMax    = 8.0  // was 5.0
	requestsPerSecondBurst  = 16.0 // was 10.0 — allow short bursts
	requestsPerSecondRoI    = 0.20 // was 0.1 — recover faster after quiet periods
	requestsPerSecondMulDec = 0.70 // was 0.5 — gentler penalty on error
)

// PollingPacketConn implements the net.PacketConn interface over an abstract
// poll function. Packets addressed to remoteAddr are passed to WriteTo are
// batched, encapsulated, and passed to the poll function. Packets addressed to
// other remote addresses are ignored. The poll function returns its own batch
// of incoming packets which are queued to be returned from a future call to
// ReadFrom.
type PollingPacketConn struct {
	remoteAddr net.Addr
	clientID   turbotunnel.ClientID
	ctx        context.Context
	cancel     context.CancelFunc
	adaptive   *AdaptiveController
	// QueuePacketConn is the direct receiver of ReadFrom and WriteTo calls.
	*turbotunnel.QueuePacketConn
}

type PollFunc func(context.Context, []byte) (io.ReadCloser, error)

func NewPollingPacketConn(remoteAddr net.Addr, poll PollFunc) *PollingPacketConn {
	clientID := turbotunnel.NewClientID()
	ctx, cancel := context.WithCancel(context.Background())
	c := &PollingPacketConn{
		remoteAddr:      remoteAddr,
		clientID:        clientID,
		ctx:             ctx,
		cancel:          cancel,
		adaptive:        NewAdaptiveController(),
		QueuePacketConn: turbotunnel.NewQueuePacketConn(clientID, 0),
	}
	go func() {
		err := c.pollLoop(poll)
		if err != nil {
			log.Printf("pollLoop: %v", err)
		}
	}()
	return c
}

// Close cancels any in-progress polls and closes the underlying QueuePacketConn.
func (c *PollingPacketConn) Close() error {
	c.cancel()
	return c.QueuePacketConn.Close()
}

func (c *PollingPacketConn) pollLoop(poll PollFunc) error {
	rateLimit := NewRateLimiter(
		time.Now(),
		requestsPerSecondMax,
		requestsPerSecondBurst,
		requestsPerSecondRoI,
	)

	// semaphore controls how many polls are in-flight simultaneously.
	// Its capacity is updated from AdaptiveController.Concurrency each loop.
	var (
		semMu  sync.Mutex
		semCap int = 3 // start at 3; grows with adaptive controller
		semCur int = 0
	)
	semAcquire := func() bool {
		semMu.Lock()
		defer semMu.Unlock()
		_, conc, _ := c.adaptive.GetParams()
		if conc != semCap {
			semCap = conc
		}
		if semCur >= semCap {
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
		pollTimerExpired := false

		// Priority: stash → outgoing → poll-timer.
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
						pollTimerExpired = true
					}
				}
			}
		}

		if pollTimerExpired {
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

		// --- Build payload ---
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

		// --- Rate / concurrency gate ---
		now := time.Now()
		// Sync rate limiter with adaptive controller's target
		_, _, targetRate := c.adaptive.GetParams()
		if rateLimit.GetRate() < targetRate {
			rateLimit.SetRate(targetRate)
		}

		if limited, _ := rateLimit.IsLimited(now); limited {
			// Drop packet to prevent stale-data build-up.
			continue
		}
		if !semAcquire() {
			// All concurrency slots busy; drop this cycle and rely on
			// the poll timer to reschedule soon.
			continue
		}
		rateLimit.Take(now, 1.0)

		payloadSnap := make([]byte, payload.Len())
		copy(payloadSnap, payload.Bytes())

		go func() {
			defer semRelease()
			start := time.Now()
			ctx, cancel := context.WithTimeout(c.ctx, pollTimeout)
			defer cancel()

			body, err := poll(ctx, payloadSnap)
			if err != nil {
				log.Printf("poll error (rate %.2f/s): %v", rateLimit.GetRate(), err)
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
				c.adaptive.RecordSuccess(int64(n), rtt)
			}
		}()
	}
}

// processIncoming reads encapsulated packets from a poll response body and
// feeds them to the incoming queue of c.QueuePacketConn.
// Returns the total number of bytes consumed from the response body.
func (c *PollingPacketConn) processIncoming(body io.Reader) (int, error) {
	// Generous limit: AMP responses are large HTML pages; 1 MB is safe.
	const maxResponseBody = 1 * 1024 * 1024
	lr := io.LimitReader(body, maxResponseBody)
	total := 0
	for {
		p, err := encapsulation.ReadData(lr)
		if err != nil {
			if err == io.EOF && lr.(*io.LimitedReader).N == 0 {
				err = errors.New("response body too large")
			} else if err == io.EOF {
				err = nil
			}
			return total, err
		}
		total += len(p)
		c.QueuePacketConn.QueueIncoming(p, c.remoteAddr)
	}
}
