package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/champa.git/armor"
	"www.bamsoftware.com/git/champa.git/encapsulation"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	// smux streams will be closed after this much time without receiving data.
	idleTimeout = 2 * time.Minute

	// How long we may wait for downstream data before sending an empty
	// response.
	maxResponseDelay = 1 * time.Second

	// How long to wait for a TCP connection to upstream to be established.
	upstreamDialTimeout = 30 * time.Second

	// net/http Server.ReadTimeout, the maximum time allowed to read an
	// entire request, including the body. Because we are likely to be
	// proxying through an AMP cache, we expect requests to be small, with
	// no streaming body.
	serverReadTimeout = 10 * time.Second
	// net/http Server.WriteTimeout, the maximum time allowed to write an
	// entire response, including the body. Because we are likely to be
	// proxying through an AMP cache, our responses are limited in size and
	// not streaming.
	serverWriteTimeout = 20 * time.Second
	// net/http Server.IdleTimeout, how long to keep a keep-alive HTTP
	// connection open, awaiting another request.
	serverIdleTimeout = idleTimeout
)

// handleStream bidirectionally connects a client stream with a TCP socket
// addressed by upstream.
func handleStream(stream *smux.Stream, upstream string, conv uint32) error {
	dialer := net.Dialer{
		Timeout: upstreamDialTimeout,
	}
	upstreamConn, err := dialer.Dial("tcp", upstream)
	if err != nil {
		return fmt.Errorf("stream %08x:%d connect upstream: %v", conv, stream.ID(), err)
	}
	defer upstreamConn.Close()
	upstreamTCPConn := upstreamConn.(*net.TCPConn)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(stream, upstreamTCPConn)
		if err == io.EOF {
			// smux Stream.Write may return io.EOF.
			err = nil
		}
		if err != nil {
			log.Printf("stream %08x:%d copy stream←upstream: %v", conv, stream.ID(), err)
		}
		upstreamTCPConn.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(upstreamTCPConn, stream)
		if err == io.EOF {
			// smux Stream.WriteTo may return io.EOF.
			err = nil
		}
		if err != nil && err != io.ErrClosedPipe {
			log.Printf("stream %08x:%d copy upstream←stream: %v", conv, stream.ID(), err)
		}
		upstreamTCPConn.CloseWrite()
	}()
	wg.Wait()

	return nil
}

// acceptStreams wraps a KCP session in a Noise channel and an smux.Session,
// then awaits smux streams. It passes each stream to handleStream.
func acceptStreams(conn *kcp.UDPSession, upstream string) error {
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	sess, err := smux.Server(conn, smuxConfig)
	if err != nil {
		return err
	}
	defer sess.Close()

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			if err == io.ErrClosedPipe {
				// We don't want to report this error.
				err = nil
			}
			return err
		}
		log.Printf("begin stream %08x:%d", conn.GetConv(), stream.ID())
		go func() {
			defer func() {
				log.Printf("end stream %08x:%d", conn.GetConv(), stream.ID())
				stream.Close()
			}()
			err := handleStream(stream, upstream, conn.GetConv())
			if err != nil {
				log.Printf("stream %08x:%d handleStream: %v", conn.GetConv(), stream.ID(), err)
			}
		}()
	}
}

// acceptSessions listens for incoming KCP connections and passes them to
// acceptStreams.
func acceptSessions(ln *kcp.Listener, upstream string) error {
	for {
		conn, err := ln.AcceptKCP()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		log.Printf("begin session %08x", conn.GetConv())
		// Permit coalescing the payloads of consecutive sends.
		conn.SetStreamMode(true)
		// Disable the dynamic congestion window (limit only by the
		// maximum of local and remote static windows).
		conn.SetNoDelay(
			0, // default nodelay
			0, // default interval
			0, // default resend
			1, // nc=1 => congestion window off
		)
		go func() {
			defer func() {
				log.Printf("end session %08x", conn.GetConv())
				conn.Close()
			}()
			err := acceptStreams(conn, upstream)
			if err != nil {
				log.Printf("session %08x acceptStreams: %v", conn.GetConv(), err)
			}
		}()
	}
}

type Handler struct {
	pconn *turbotunnel.QueuePacketConn
}

// decodeRequest extracts a ClientID and a payload from an incoming HTTP
// request. In case of a decoding failure, the returned payload slice will be
// nil. The payload is always non-nil after a successful decoding, even if the
// payload is empty.
func decodeRequest(req *http.Request) (turbotunnel.ClientID, []byte) {
	if req.Method != "GET" {
		return turbotunnel.ClientID{}, nil
	}

	var clientID turbotunnel.ClientID
	var payload []byte

	// Check the version indicator of the incoming client–server protocol.
	switch {
	case strings.HasPrefix(req.URL.Path, "/0"):
		// Version "0"'s payload is base64-encoded, using the URL-safe
		// alphabet without padding, in the final path component
		// (earlier path components are ignored).
		var err error
		_, encoded := path.Split(req.URL.Path[2:]) // Remove version prefix.
		payload, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return turbotunnel.ClientID{}, nil
		}
		n := copy(clientID[:], payload)
		payload = payload[n:]
		if n != len(clientID) {
			return turbotunnel.ClientID{}, nil
		}
	default:
		return turbotunnel.ClientID{}, nil
	}

	return clientID, payload
}

func (handler *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	const maxPayloadLength = 5000

	clientID, payload := decodeRequest(req)
	if payload == nil {
		http.Error(rw, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// Read incoming packets from the payload.
	r := bytes.NewReader(payload)
	for {
		p, err := encapsulation.ReadData(r)
		if err != nil {
			break
		}
		handler.pconn.QueueIncoming(p, clientID)
	}

	rw.Header().Set("Content-Type", "text/html")
	// Attempt to hint to an AMP cache not to waste resources caching this
	// document. "The Google AMP Cache considers any document fresh for at
	// least 15 seconds."
	// https://developers.google.com/amp/cache/overview#google-amp-cache-updates
	rw.Header().Set("Cache-Control", "max-age=15")
	rw.WriteHeader(http.StatusOK)

	enc, err := armor.NewEncoder(rw)
	if err != nil {
		log.Printf("armor.NewEncoder: %v", err)
		return
	}
	defer enc.Close()

	limit := maxPayloadLength
	// We loop and bundle as many outgoing packets as will fit, up to
	// maxPayloadLength. We wait up to maxResponseDelay for the first
	// available packet; after that we only include whatever packets are
	// immediately available.
	timer := time.NewTimer(maxResponseDelay)
	defer timer.Stop()
	first := true
	for {
		var p []byte
		unstash := handler.pconn.Unstash(clientID)
		outgoing := handler.pconn.OutgoingQueue(clientID)
		// Prioritize taking a packet from the stash before checking the
		// outgoing queue.
		select {
		case p = <-unstash:
		case <-timer.C:
		default:
			select {
			case p = <-unstash:
			case p = <-outgoing:
			case <-timer.C:
			}
		}
		// We wait for the first packet only. Later packets must be
		// immediately available.
		timer.Reset(0)

		if len(p) == 0 {
			// Timer expired, we are done bundling packets into this
			// response.
			break
		}

		limit -= len(p)
		if !first && limit < 0 {
			// This packet doesn't fit in the payload size limit.
			// Stash it so that it will be first in line for the
			// next response.
			handler.pconn.Stash(p, clientID)
			break
		}
		first = false

		// Write the packet to the AMP response.
		_, err := encapsulation.WriteData(enc, p)
		if err != nil {
			log.Printf("encapsulation.WriteData: %v", err)
			break
		}
		if rw, ok := rw.(http.Flusher); ok {
			rw.Flush()
		}
	}
}

func run(listen, upstream string) error {
	// Start up the virtual PacketConn for turbotunnel.
	pconn := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, idleTimeout*2)
	ln, err := kcp.ServeConn(nil, 0, 0, pconn)
	if err != nil {
		return fmt.Errorf("opening KCP listener: %v", err)
	}
	defer ln.Close()
	go func() {
		err := acceptSessions(ln, upstream)
		if err != nil {
			log.Printf("acceptSessions: %v", err)
		}
	}()

	handler := &Handler{
		pconn: pconn,
	}

	server := &http.Server{
		Addr:         listen,
		Handler:      handler,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
		// The default MaxHeaderBytes is plenty for our purposes.
	}
	defer server.Close()

	return server.ListenAndServe()
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s LISTENADDR UPSTREAMADDR

Example:
  %[1]s 127.0.0.1:8080 127.0.0.1:7001

`, os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	listen := flag.Arg(0)
	upstream := flag.Arg(1)
	// We keep upstream as a string in order to eventually pass it to
	// net.Dial in handleStream. But we do a preliminary resolution of the
	// name here, in order to exit with a quick error at startup if the
	// address cannot be parsed or resolved.
	{
		upstreamTCPAddr, err := net.ResolveTCPAddr("tcp", upstream)
		if err == nil && upstreamTCPAddr.IP == nil {
			err = fmt.Errorf("missing host in address")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot parse upstream address: %v\n", err)
			os.Exit(1)
		}
	}

	err := run(listen, upstream)
	if err != nil {
		log.Fatal(err)
	}
}
