package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"time"

	"www.bamsoftware.com/git/champa.git/encapsulation"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	initPollDelay                          = 500 * time.Millisecond  // سریع‌تر
	maxPollDelay                           = 5 * time.Second          // کوتاه‌تر
	pollDelayMultiplier                    = 1.5                      // نرم‌تر
	pollTimeout                            = 30 * time.Second
	requestsPerSecondMax                   = 20.0                     // 4x بیشتر
	requestsPerSecondBurst                 = 40.0
	requestsPerSecondRateOfIncrease        = 2.0 / 10.0               // سریع‌تر
	requestsPerSecondMultiplicativeDecrease = 0.7                     // نرم‌تر
)

type PollingPacketConn struct {
	remoteAddr net.Addr
	clientID   turbotunnel.ClientID
	ctx        context.Context
	cancel     context.CancelFunc
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

func (c *PollingPacketConn) Close() error {
	c.cancel()
	return c.QueuePacketConn.Close()
}

func (c *PollingPacketConn) pollLoop(poll PollFunc) error {
	const maxPayloadLength = 2048

	rateLimit := NewRateLimiter(
		time.Now(),
		requestsPerSecondMax,
		requestsPerSecondBurst,
		requestsPerSecondRateOfIncrease,
	)

	pollDelay := initPollDelay
	pollTimer := time.NewTimer(pollDelay)
	
	defer func() {
		if !pollTimer.Stop() {
			<-pollTimer.C
		}
	}()
	
	for {
		var p []byte
		unstash := c.QueuePacketConn.Unstash(c.remoteAddr)
		outgoing := c.QueuePacketConn.OutgoingQueue(c.remoteAddr)
		pollTimerExpired := false

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
				<-pollTimer.C
			}
			pollDelay = initPollDelay
		}
		pollTimer.Reset(pollDelay)

		var payload bytes.Buffer
		payload.Write(c.clientID[:])

		first := true
		for len(p) > 0 && (first || payload.Len()+len(p) <= maxPayloadLength) {
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

		now := time.Now()
		if limited, _ := rateLimit.IsLimited(now); limited {
			// حذف بسته‌های قدیمی برای جلوگیری از buffering
		} else {
			rateLimit.Take(now, 1.0)

			go func() {
				ctx, cancel := context.WithTimeout(c.ctx, pollTimeout)
				defer cancel()
				
				body, err := poll(ctx, payload.Bytes())
				if err != nil {
					log.Printf("poll error, reducing request rate from %.3f/s: %v", rateLimit.rate, err)
					rateLimit.MultiplicativeDecrease(now, requestsPerSecondMultiplicativeDecrease)
					return
				}
				defer body.Close()
				
				err = c.processIncoming(body)
				if err != nil {
					log.Printf("processIncoming: %v", err)
				}
			}()
		}
	}
}

func (c *PollingPacketConn) processIncoming(body io.Reader) error {
	lr := io.LimitReader(body, 500*1024)
	for {
		p, err := encapsulation.ReadData(lr)
		if err != nil {
			if err == io.EOF && lr.(*io.LimitedReader).N == 0 {
				err = errors.New("response body too large")
			} else if err == io.EOF {
				err = nil
			}
			return err
		}

		c.QueuePacketConn.QueueIncoming(p, c.remoteAddr)
	}
}
