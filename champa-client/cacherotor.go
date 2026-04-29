package main

import (
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// Number of quota errors on one cache before rotating to the next.
	cacheRotateThreshold = 15

	// Errors older than this window are discarded (sliding window).
	cacheErrorWindow = 15 * time.Minute
)

// CacheRotor cycles through a list of AMP cache base URLs. Each provider
// maintains an independent quota counter, so rotating when one provider's
// quota is exhausted extends total session lifetime proportionally.
//
// Thread-safe: all methods acquire the internal mutex.
type CacheRotor struct {
	mu       sync.Mutex
	caches   []*url.URL // nil entry = direct mode (no cache)
	cur      int
	errCount []int
	lastErr  []time.Time
}

// NewCacheRotor parses rawURLs into a ready CacheRotor.
//   - Empty or whitespace-only strings are treated as "direct" (no-cache) slots.
//   - Unparseable entries are logged and skipped.
//   - If the resulting list is empty a single nil (direct-mode) slot is used.
func NewCacheRotor(rawURLs []string) *CacheRotor {
	var urls []*url.URL
	for _, s := range rawURLs {
		s = strings.TrimSpace(s)
		if s == "" {
			urls = append(urls, nil)
			continue
		}
		u, err := url.Parse(s)
		if err != nil {
			log.Printf("[rotor] skipping invalid cache URL %q: %v", s, err)
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		urls = []*url.URL{nil}
	}
	return &CacheRotor{
		caches:   urls,
		errCount: make([]int, len(urls)),
		lastErr:  make([]time.Time, len(urls)),
	}
}

// Current returns the active cache URL.
// Returns nil when the active slot is direct (no-cache) mode.
func (r *CacheRotor) Current() *url.URL {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caches[r.cur]
}

// CurrentName returns a human-readable name for the active cache.
func (r *CacheRotor) CurrentName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nameAt(r.cur)
}

// nameAt returns the display name for slot i. Caller must hold r.mu.
func (r *CacheRotor) nameAt(i int) string {
	if r.caches[i] == nil {
		return "direct"
	}
	return r.caches[i].Host
}

// RecordQuotaError records one quota error against the active cache.
// When the error threshold is reached within the sliding window the rotor
// advances to the next cache and resets that cache's counter.
func (r *CacheRotor) RecordQuotaError() {
	r.mu.Lock()
	defer r.mu.Unlock()

	i := r.cur
	// Discard stale errors outside the sliding window.
	if !r.lastErr[i].IsZero() && time.Since(r.lastErr[i]) > cacheErrorWindow {
		r.errCount[i] = 0
	}
	r.errCount[i]++
	r.lastErr[i] = time.Now()

	if r.errCount[i] < cacheRotateThreshold || len(r.caches) <= 1 {
		return
	}

	next := (r.cur + 1) % len(r.caches)
	log.Printf("[rotor] cache %s hit quota threshold (%d errors) -- rotating to %s",
		r.nameAt(i), r.errCount[i], r.nameAt(next))
	r.cur = next
	r.errCount[r.cur] = 0
}

// RecordSuccess resets the error counter for the active cache.
func (r *CacheRotor) RecordSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errCount[r.cur] = 0
}
