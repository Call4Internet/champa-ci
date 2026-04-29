package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"www.bamsoftware.com/git/champa.git/amp"
	"www.bamsoftware.com/git/champa.git/armor"
)

// ErrCacheQuota is returned when the AMP cache signals quota exhaustion via
// HTTP 302/301 or a "silent redirect" (200 + Location header).
// Callers must back off; falling back to direct mode is NOT safe because the
// origin server may be unreachable without domain fronting.
var ErrCacheQuota = errors.New("AMP cache quota exceeded")

// cacheBreaker returns 12 random bytes that are embedded in every request URL
// to prevent the AMP cache from serving a stale cached response.
func cacheBreaker() []byte {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// exchangeAMP encodes payload p into a URL, sends it to serverURL through
// cacheURL (nil = direct, no AMP cache), and returns the decoded response body.
//
// front, when non-empty, replaces the TLS SNI and HTTP Host header so that the
// connection appears to go to a different domain (domain fronting).
func exchangeAMP(
	ctx context.Context,
	rt http.RoundTripper,
	serverURL, cacheURL *url.URL,
	front string,
	p []byte,
) (io.ReadCloser, error) {
	// Build the request URL: /0<cache-breaker>/<payload-b64>
	u := serverURL.ResolveReference(&url.URL{
		Path: strings.Join([]string{
			"0" + base64.RawURLEncoding.EncodeToString(cacheBreaker()),
			base64.RawURLEncoding.EncodeToString(p),
		}, "/"),
	})

	if cacheURL != nil {
		var err error
		u, err = amp.CacheURL(u, cacheURL, "c")
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	// Apply domain fronting: replace the Host/SNI while keeping the path.
	if front != "" {
		if _, port, err := net.SplitHostPort(req.URL.Host); err == nil {
			req.URL.Host = net.JoinHostPort(front, port)
		} else {
			req.URL.Host = front
		}
	}

	// Empty User-Agent avoids AMP cache classification.
	req.Header.Set("User-Agent", "")
	// Do NOT set Accept-Encoding. Go auto-decompresses only when it added the
	// header itself; the armor layer handles its own encoding.

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 302 / 301 from the AMP cache means quota / rate-limit redirect.
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %v", ErrCacheQuota, resp.Status)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("server returned status %v", resp.Status)
	}

	// "Silent redirect": status 200 with a Location header and a JS body.
	if _, err := resp.Location(); err == nil {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: silent redirect", ErrCacheQuota)
	}

	dec, err := armor.NewDecoder(bufio.NewReader(resp.Body))
	if err != nil {
		resp.Body.Close()
		return nil, err
	}

	// Combine the armor decoder (Reader) with the HTTP body (Closer).
	return struct {
		io.Reader
		io.Closer
	}{dec, resp.Body}, nil
}
