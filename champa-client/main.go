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

const idleTimeout = 2 * time.Minute

// ==================== CACHING LAYER ====================

type ResponseCache struct {
	mu    sync.RWMutex
	cache map[string][]byte
	ttl   map[string]time.Time
}

func NewResponseCache() *ResponseCache {
	return &ResponseCache{
		cache: make(map[string][]byte),
		ttl:   make(map[string]time.Time),
	}
}

func (rc *ResponseCache) Get(key string) ([]byte, bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	
	if expiry, exists := rc.ttl[key]; exists {
		if time.Now().Before(expiry) {
			return rc.cache[key], true
		}
	}
	return nil, false
}

func (rc *ResponseCache) Set(key string, value []byte, ttl time.Duration) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	
	rc.cache[key] = value
	rc.ttl[key] = time.Now().Add(ttl)
}

func (rc *ResponseCache) Clear() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	
	rc.cache = make(map[string][]byte)
	rc.ttl = make(map[string]time.Time)
}

// ==================== OPTIMIZED RATE LIMITER ====================

const (
	// بهتر و سریع‌تر از قبل
	initPollDelay                          = 500 * time.Millisecond
	maxPollDelay                           = 5 * time.Second
	pollDelayMultiplier                    = 1.5
	pollTimeout                            = 30 * time.Second
	requestsPerSecondMax                   = 20.0      // 4x بیشتر
	requestsPerSecondBurst                 = 40.0
	requestsPerSecondRateOfIncrease        = 2.0 / 10.0 // 2x سریع‌تر
	requestsPerSecondMultiplicativeDecrease = 0.7      // کمتر صحت‌پذیر
	
	// Connection pooling
	maxConnsPerHost = 20 // از 2 به 20
	maxIdleConns    = 50
	
	// Buffer sizes
	maxReceiveBuffer = 64 * 1024 * 1024 // از 4MB به 64MB
	maxStreamBuffer  = 16 * 1024 * 1024 // از 1MB به 16MB
	
	// Concurrent polls
	concurrentPolls = 5 // چند poll همزمان
)

// ==================== CONCURRENT POLLING ====================

type ConcurrentPoller struct {
	poll        PollFunc
	cache       *ResponseCache
	rateLimiter *RateLimiter
	semaphore   chan struct{} // محدود کردن تعداد polls
	mu          sync.Mutex
}

func NewConcurrentPoller(poll PollFunc, cache *ResponseCache, rl *RateLimiter) *ConcurrentPoller {
	return &ConcurrentPoller{
		poll:        poll,
		cache:       cache,
		rateLimiter: rl,
		semaphore:   make(chan struct{}, concurrentPolls),
	}
}

func (cp *ConcurrentPoller) Poll(ctx context.Context, payload []byte) (io.ReadCloser, error) {
	// چک کردن cache
	cacheKey := fmt.Sprintf("%x", payload)
	if cached, ok := cp.cache.Get(cacheKey); ok {
		return io.NopCloser(bytes.NewReader(cached)), nil
	}
	
	// سمافور برای محدود کردن concurrent polls
	select {
	case cp.semaphore <- struct{}{}:
		defer func() { <-cp.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	
	body, err := cp.poll(ctx, payload)
	if err != nil {
		return nil, err
	}
	
	// کش کردن نتیجه
	data, err := io.ReadAll(body)
	if err != nil {
		body.Close()
		return nil, err
	}
	body.Close()
	
	cp.cache.Set(cacheKey, data, 30*time.Second)
	
	return io.NopCloser(bytes.NewReader(data)), nil
}

// ==================== ENHANCED POLLING PACKET CONN ====================

type EnhancedPollingPacketConn struct {
	*PollingPacketConn
	poller *ConcurrentPoller
}

func NewEnhancedPollingPacketConn(remoteAddr net.Addr, poll PollFunc, cache *ResponseCache, rl *RateLimiter) *EnhancedPollingPacketConn {
	poller := NewConcurrentPoller(poll, cache, rl)
	return &EnhancedPollingPacketConn{
		PollingPacketConn: NewPollingPacketConn(remoteAddr, poller.Poll),
		poller:           poller,
	}
}

// ==================== OPTIMIZED RUN FUNCTION ====================

func run(rt http.RoundTripper, serverURL, cacheURL *url.URL, front, localAddr string, pubkey []byte) error {
	// بهتر‌سازی HTTP transport
	transport := &http.Transport{
		MaxConnsPerHost:       maxConnsPerHost,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
	}
	
	if t, ok := rt.(*http.Transport); ok {
		t.MaxConnsPerHost = maxConnsPerHost
		t.MaxIdleConns = maxIdleConns
	}
	
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}
	defer ln.Close()
	
	// Cache layer
	responseCache := NewResponseCache()
	defer responseCache.Clear()
	
	// Rate limiter
	rateLimit := NewRateLimiter(
		time.Now(),
		requestsPerSecondMax,
		requestsPerSecondBurst,
		requestsPerSecondRateOfIncrease,
	)
	
	var poll PollFunc = func(ctx context.Context, p []byte) (io.ReadCloser, error) {
		return exchangeAMP(ctx, transport, serverURL, cacheURL, front, p)
	}
	
	// Enhanced polling with concurrency
	pconn := NewEnhancedPollingPacketConn(turbotunnel.DummyAddr{}, poll, responseCache, &rateLimit)
	defer pconn.Close()
	
	// Noise encryption layer
	nconn, err := noiseDial(pconn, turbotunnel.DummyAddr{}, pubkey)
	if err != nil {
		return err
	}
	
	// KCP connection with optimized settings
	conn, err := kcp.NewConn2(turbotunnel.DummyAddr{}, nil, 0, 0, nconn)
	if err != nil {
		return fmt.Errorf("opening KCP conn: %v", err)
	}
	defer func() {
		log.Printf("end session %08x", conn.GetConv())
		conn.Close()
	}()
	
	log.Printf("begin session %08x", conn.GetConv())
	
	// KCP optimization
	conn.SetStreamMode(true)
	conn.SetNoDelay(0, 0, 0, 1)
	conn.SetACKNoDelay(true)
	conn.SetWindowSize(2048, 2048) // از 1024 به 2048
	
	// smux configuration with larger buffers
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	smuxConfig.MaxReceiveBuffer = maxReceiveBuffer
	smuxConfig.MaxStreamBuffer = maxStreamBuffer
	
	sess, err := smux.Client(conn, smuxConfig)
	if err != nil {
		return fmt.Errorf("opening smux session: %v", err)
	}
	defer sess.Close()
	
	// Accept loop
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
	var pubkeyFilename string
	var pubkeyString string
	
	flag.StringVar(&cache, "cache", "", "URL of AMP cache")
	flag.StringVar(&front, "front", "", "domain to domain-front")
	flag.StringVar(&pubkeyString, "pubkey", "", "server public key")
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
