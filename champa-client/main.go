package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

// smux streams will be closed after this much time without receiving data.
const idleTimeout = 2 * time.Minute

func handle(local *net.TCPConn, sess *smux.Session, conv uint32) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("session %08x opening stream: %v", conv, err)
	}
	defer func() {
		log.Printf("end stream %08x:%d", conv, stream.ID())
		stream.Close()
	}()
	log.Printf("begin stream %08x:%d", conv, stream.ID())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(stream, local)
		if err == io.EOF {
			// smux Stream.Write may return io.EOF.
			err = nil
		}
		if err != nil {
			log.Printf("stream %08x:%d copy stream←local: %v", conv, stream.ID(), err)
		}
		local.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, stream)
		if err == io.EOF {
			// smux Stream.WriteTo may return io.EOF.
			err = nil
		}
		if err != nil && err != io.ErrClosedPipe {
			log.Printf("stream %08x:%d copy local←stream: %v", conv, stream.ID(), err)
		}
		local.CloseWrite()
	}()
	wg.Wait()

	return err
}

func run(serverURL, cacheURL *url.URL, front, localAddr string) error {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	var poll PollFunc = func(p []byte) (io.ReadCloser, error) {
		return exchangeAMP(serverURL, cacheURL, front, p)
	}
	pconn := NewPollingPacketConn(poll)
	defer pconn.Close()

	// Open a KCP conn on the PacketConn.
	conn, err := kcp.NewConn2(turbotunnel.DummyAddr{}, nil, 0, 0, pconn)
	if err != nil {
		return fmt.Errorf("opening KCP conn: %v", err)
	}
	defer func() {
		log.Printf("end session %08x", conn.GetConv())
		conn.Close()
	}()
	log.Printf("begin session %08x", conn.GetConv())
	// Permit coalescing the payloads of consecutive sends.
	conn.SetStreamMode(true)
	// Disable the dynamic congestion window (limit only by the maximum of
	// local and remote static windows).
	conn.SetNoDelay(
		0, // default nodelay
		0, // default interval
		0, // default resend
		1, // nc=1 => congestion window off
	)
	// TODO: We could optimize a call to conn.SetMtu here, based on a
	// maximum URL length we want to send (such as the 8000 bytes
	// recommended at https://datatracker.ietf.org/doc/html/rfc7230#section-3.1.1).
	// The idea is that if we can slightly reduce the MTU from its default
	// to permit one more packet per request, we should do it.
	// E.g. 1400*5 = 7000, but 1320*6 = 7920.

	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	sess, err := smux.Client(conn, smuxConfig)
	if err != nil {
		return fmt.Errorf("opening smux session: %v", err)
	}
	defer sess.Close()

	for {
		local, err := ln.Accept()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		go func() {
			defer local.Close()
			err := handle(local.(*net.TCPConn), sess, conn.GetConv())
			if err != nil {
				log.Printf("handle: %v", err)
			}
		}()
	}
}

func main() {
	var cache string
	var front string

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s [-cache CACHEURL] [-front DOMAIN] SERVERURL LOCALADDR

Example:
  %[1]s -cache https://amp.cache.example/ -front amp.cache.example https://server.example/champa/ 127.0.0.1:7000

`, os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&cache, "cache", "", "URL of AMP cache")
	flag.StringVar(&front, "front", "", "domain to domain-front HTTPS requests with")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	serverURL, err := url.Parse(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot parse server URL: %v\n", err)
		os.Exit(1)
	}
	localAddr := flag.Arg(1)

	var cacheURL *url.URL
	if cache != "" {
		cacheURL, err = url.Parse(cache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot parse AMP cache URL: %v\n", err)
			os.Exit(1)
		}
	}

	err = run(serverURL, cacheURL, front, localAddr)
	if err != nil {
		log.Fatal(err)
	}
}
