package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/champa.git/noise"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	idleTimeout    = 2 * time.Minute
	reconnectDelay = 5 * time.Second
)

func readKeyFromFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return noise.ReadKey(f)
}

type noisePacketConn struct {
	sess *noise.Session
	net.PacketConn
}

func readNoiseMessageOfTypeFrom(conn net.PacketConn, wantedType byte) ([]byte, net.Addr, error) {
	for {
		msgType, msg, addr, err := noise.ReadMessageFrom(conn)
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return nil, nil, err
		}
		if msgType == wantedType {
			return msg, addr, nil
		}
	}
}

func noiseDial(conn net.PacketConn, addr net.Addr, pubkey []byte) (*noisePacketConn, error) {
	p := []byte{noise.MsgTypeHandshakeInit}
	pre, p, err := noise.InitiateHandshake(p, pubkey)
	if err != nil {
		return nil, err
	}
	_, err = conn.WriteTo(p, addr)
	if err != nil {
		return nil, err
	}
	msg, _, err := readNoiseMessageOfTypeFrom(conn, noise.MsgTypeHandshakeResp)
	if err != nil {
		return nil, err
	}
	sess, err := pre.FinishHandshake(msg)
	if err != nil {
		return nil, err
	}
	return &noisePacketConn{sess, conn}, nil
}

func (c *noisePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	msg, addr, err := readNoiseMessageOfTypeFrom(c.PacketConn, noise.MsgTypeTransport)
	if err != nil {
		return 0, nil, err
	}
	dec, err := c.sess.Decrypt(nil, msg)
	if errors.Is(err, noise.ErrInvalidNonce) {
		log.Printf("%v", err)
		return 0, addr, nil
	} else if err != nil {
		return 0, nil, err
	}
	return copy(p, dec), addr, nil
}

func (c *noisePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	buf := []byte{noise.MsgTypeTransport}
	buf, err := c.sess.Encrypt(buf, p)
	if err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(buf, addr)
}

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
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("stream %08x:%d copy stream←local: %v", conv, stream.ID(), err)
		}
		local.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, stream)
		if err == io.EOF {
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("stream %08x:%d copy local←stream: %v", conv, stream.ID(), err)
		}
		local.CloseWrite()
	}()
	wg.Wait()
	return nil
}

// dialSession builds polling→Noise→KCP→smux and returns a ready session.
func dialSession(rt http.RoundTripper, serverURL, cacheURL *url.URL, front string, pubkey []byte) (*smux.Session, *PollingPacketConn, uint32, error) {
	var poll PollFunc = func(ctx context.Context, p []byte) (io.ReadCloser, error) {
		return exchangeAMP(ctx, rt, serverURL, cacheURL, front, p)
	}
	pconn := NewPollingPacketConn(turbotunnel.DummyAddr{}, poll)

	nconn, err := noiseDial(pconn, turbotunnel.DummyAddr{}, pubkey)
	if err != nil {
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("noise handshake: %v", err)
	}

	conn, err := kcp.NewConn2(turbotunnel.DummyAddr{}, nil, 0, 0, nconn)
	if err != nil {
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("opening KCP conn: %v", err)
	}
	conv := conn.GetConv()
	log.Printf("begin session %08x", conv)

	conn.SetStreamMode(true)
	conn.SetNoDelay(1, 10, 2, 1)
	conn.SetACKNoDelay(true)
	conn.SetWindowSize(2048, 2048)

	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	smuxConfig.MaxReceiveBuffer = 8 * 1024 * 1024
	smuxConfig.MaxStreamBuffer = 2 * 1024 * 1024
	sess, err := smux.Client(conn, smuxConfig)
	if err != nil {
		conn.Close()
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("opening smux session: %v", err)
	}
	return sess, pconn, conv, nil
}

// sessionHolder keeps one live smux session and reconnects automatically.
type sessionHolder struct {
	mu   sync.RWMutex
	sess *smux.Session
	conv uint32

	rt        http.RoundTripper
	serverURL *url.URL
	cacheURL  *url.URL
	front     string
	pubkey    []byte
}

func newSessionHolder(rt http.RoundTripper, serverURL, cacheURL *url.URL, front string, pubkey []byte) *sessionHolder {
	h := &sessionHolder{
		rt: rt, serverURL: serverURL, cacheURL: cacheURL,
		front: front, pubkey: pubkey,
	}
	go h.keepAlive()
	return h
}

// keepAlive dials until it succeeds, then waits for the session to close,
// and dials again. Runs forever in the background.
func (h *sessionHolder) keepAlive() {
	for {
		sess, pconn, conv, err := dialSession(h.rt, h.serverURL, h.cacheURL, h.front, h.pubkey)
		if err != nil {
			log.Printf("dial failed (retry in %v): %v", reconnectDelay, err)
			time.Sleep(reconnectDelay)
			continue
		}

		h.mu.Lock()
		h.sess = sess
		h.conv = conv
		h.mu.Unlock()

		// Block until the session is closed, then loop to reconnect.
		for !sess.IsClosed() {
			time.Sleep(500 * time.Millisecond)
		}
		pconn.Close()
		log.Printf("end session %08x — reconnecting in %v…", conv, reconnectDelay)
		time.Sleep(reconnectDelay)
	}
}

func (h *sessionHolder) get() (*smux.Session, uint32) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sess, h.conv
}

func run(rt http.RoundTripper, serverURL, cacheURL *url.URL, front, localAddr string, pubkey []byte) error {
	http.DefaultTransport.(*http.Transport).MaxConnsPerHost = 8

	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	holder := newSessionHolder(rt, serverURL, cacheURL, front, pubkey)

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
			sess, conv := holder.get()
			if sess == nil || sess.IsClosed() {
				log.Printf("session not ready, dropping %v", local.RemoteAddr())
				return
			}
			if err := handle(local.(*net.TCPConn), sess, conv); err != nil {
				log.Printf("handle: %v", err)
			}
		}()
	}
}

func main() {
	var cache string
	var front string
	var pubkeyFilename string
	var pubkeyString string

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s -pubkey-file PUBKEYFILE [-cache CACHEURL] [-front DOMAIN] SERVERURL LOCALADDR

Example:
  %[1]s -pubkey-file server.pub -cache https://cdn.ampproject.org/ -front www.google.com https://server.example/champa/ 127.0.0.1:7000

`, os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&cache, "cache", "", "URL of AMP cache (try https://cdn.ampproject.org/)")
	flag.StringVar(&front, "front", "", "domain to domain-front HTTPS requests with (try www.google.com)")
	flag.StringVar(&pubkeyString, "pubkey", "", fmt.Sprintf("server public key (%d hex digits)", noise.KeyLen*2))
	flag.StringVar(&pubkeyFilename, "pubkey-file", "", "read server public key from file")
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

	var pubkey []byte
	if pubkeyFilename != "" && pubkeyString != "" {
		fmt.Fprintf(os.Stderr, "only one of -pubkey and -pubkey-file may be used\n")
		os.Exit(1)
	} else if pubkeyFilename != "" {
		pubkey, err = readKeyFromFile(pubkeyFilename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot read pubkey from file: %v\n", err)
			os.Exit(1)
		}
	} else if pubkeyString != "" {
		pubkey, err = noise.DecodeKey(pubkeyString)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pubkey format error: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Fprintf(os.Stderr, "the -pubkey or -pubkey-file option is required\n")
		os.Exit(1)
	}

	err = run(http.DefaultTransport, serverURL, cacheURL, front, localAddr, pubkey)
	if err != nil {
		log.Fatal(err)
	}
}
