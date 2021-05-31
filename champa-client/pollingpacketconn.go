package main

import (
	"io"
	"io/ioutil"
	"log"
	"time"

	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	// pollLoop has a poll timer that automatically sends an empty polling
	// query when a certain amount of time has elapsed without a send. The
	// poll timer is initially set to initPollDelay. It increases by a
	// factor of pollDelayMultiplier every time the poll timer expires, up
	// to a maximum of maxPollDelay. The poll timer is reset to
	// initPollDelay whenever an a send occurs that is not the result of the
	// poll timer expiring.
	initPollDelay       = 500 * time.Millisecond
	maxPollDelay        = 10 * time.Second
	pollDelayMultiplier = 2.0
)

// PollingPacketConn implements the net.PacketConn interface over an abstract
// poll function. Packets passed to WriteTo are batched, encapsulated, and
// passed to the poll function. The poll function returns its own batch of
// incoming packets which are queued to be returned from a future call to
// ReadFrom.
type PollingPacketConn struct {
	clientID turbotunnel.ClientID
	// Sending on pollChan permits pollLoop to send an empty polling query.
	// pollLoop also does its own polling according to a time schedule.
	pollChan chan struct{}
	// QueuePacketConn is the direct receiver of ReadFrom and WriteTo calls.
	// sendLoop, via send, removes messages from the outgoing queue that
	// were placed there by WriteTo, and inserts messages into the incoming
	// queue to be returned from ReadFrom.
	*turbotunnel.QueuePacketConn
}

type PollFunc func([]byte) (io.ReadCloser, error)

func NewPollingPacketConn(poll PollFunc) *PollingPacketConn {
	clientID := turbotunnel.NewClientID()
	c := &PollingPacketConn{
		clientID:        clientID,
		pollChan:        make(chan struct{}),
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

func (c *PollingPacketConn) pollLoop(poll PollFunc) error {
	pollDelay := initPollDelay
	pollTimer := time.NewTimer(pollDelay)
	for {
		var p []byte
		outgoingQueue := c.QueuePacketConn.OutgoingQueue(turbotunnel.DummyAddr{})
		pollTimerExpired := false
		// Prioritize sending an actual data packet from OutgoingQueue.
		// Only consider a poll when OutgoingQueue is empty.
		select {
		case p = <-outgoingQueue:
		default:
			select {
			case p = <-outgoingQueue:
			case <-c.pollChan:
				p = nil
			case <-pollTimer.C:
				p = nil
				pollTimerExpired = true
			}
		}

		if len(p) > 0 {
			// A data-carrying packet displaces one pending poll
			// opportunity, if any.
			select {
			case <-c.pollChan:
			default:
			}
		}

		if pollTimerExpired {
			// We're polling because it's been a while since we last
			// polled. Increase the poll delay.
			pollDelay = time.Duration(float64(pollDelay) * pollDelayMultiplier)
			if pollDelay > maxPollDelay {
				pollDelay = maxPollDelay
			}
		} else {
			// We're sending an actual data packet, or we're polling
			// in response to a received packet. Reset the poll
			// delay to initial.
			if !pollTimer.Stop() {
				<-pollTimer.C
			}
			pollDelay = initPollDelay
		}
		pollTimer.Reset(pollDelay)

		// TODO batching
		go func() {
			body, err := poll(append(c.clientID[:], p...))
			if err != nil {
				log.Printf("poll: %v", err)
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

// processIncoming reads a packet from an HTTP response body and feeds it to the
// incoming queue of c.QueuePacketConn.
//
// Whenever we receive a response with a non-empty payload, we send twice on
// c.pollChan to permit sendLoop to send two immediate polling queries. The
// intuition behind polling immediately after receiving is that we know the
// server has just had something to send, it may need to send more, and the only
// way it can send is if we give it a query to respond to. The intuition behind
// doing *two* polls when we receive is similar to TCP slow start: we want to
// maintain some number of queries "in flight", and the faster the server is
// sending, the higher that number should be. If we polled only once in response
// to received data, we would tend to have only one query in flight at a time,
// ping-pong style. The first polling request replaces the in-flight request
// that has just finished in our receiving data; the second grows the effective
// in-flight window proportionally to the rate at which data-carrying responses
// are being received. Compare to Eq. (2) of
// https://tools.ietf.org/html/rfc5681#section-3.1; the differences are that we
// count messages, not bytes, and we don't maintain an explicit window. If a
// response comes back without data, or if a query or response is dropped by the
// network, then we don't poll again, which decreases the effective in-flight
// window.
func (c *PollingPacketConn) processIncoming(body io.Reader) error {
	// TODO limit size
	// TODO timeout
	x, err := ioutil.ReadAll(body)
	if err != nil {
		return err
	}

	if len(x) > 0 {
		c.QueuePacketConn.QueueIncoming(x, turbotunnel.DummyAddr{})

		// If the payload contained one or more packets, permit pollLoop
		// to poll immediately.
		select {
		case c.pollChan <- struct{}{}:
		default:
		}
		select {
		case c.pollChan <- struct{}{}:
		default:
		}
	}

	return nil
}
