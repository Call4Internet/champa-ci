package main

import (
	"bytes"
	"context"
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
	idleTimeout = 2 * time.Minute

	// Connection pooling بهتر
	maxConnsPerHost = 20
	maxIdleConns    = 50

	// Buffer sizes بزرگ‌تر
	maxReceiveBuffer = 64 * 1024 * 1024
	maxStreamBuffer  = 16 * 1024 * 1024

	// Concurrent polls
	concurrentPolls = 5
)

// ==================== RESPONSE CACHE ====================

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

// ==================== CONCURRENT POLLER ====================

type ConcurrentPoller struct {
	poll      PollFunc
	cache     *ResponseCache
	semaphore chan struct{}
	mu        sync.Mutex
}

func NewConcurrentPoller(poll PollFunc, cache *ResponseCache) *ConcurrentPoller {
	return &ConcurrentPoller{
		poll:      poll,
		cache:     cache,
		semaphore: make(chan struct{}, concurrentPolls),
	}
}

func (cp *ConcurrentPoller) Poll(ctx context.Context, payload []byte) (io.ReadCloser, error) {
	// کش چک کن
	cacheKey := fmt.Sprintf("%x", payload)
	if cached, ok := cp.cache.Get(cacheKey); ok {
		return io.NopCloser(bytes.NewReader(cached)), nil
	}

	// سمافور برای محدود کردن تعداد polls
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

	// نتیجه را کش کن
	data, err := io.ReadAll(body)
	if err != nil {
		body.Close()
		return nil, err
	}
	body.Close()

	cp.cache.Set(cacheKey, data, 30*time.Second)

	return io.NopCloser(bytes.NewReader(data)), nil
}

// ==================== RUN FUNCTION ====================

func run(rt http.RoundTripper, serverURL, cacheURL *url
