package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"www.bamsoftware.com/git/champa.git/amp"
	"www.bamsoftware.com/git/champa.git/armor"
)

// cacheBreaker returns a random byte slice used to prevent AMP cache
// from serving a cached (stale) response.
func cacheBreaker() []byte {
	buf := make([]byte, 12)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}
	return buf
}

func exchangeAMP(ctx context.Context, rt http.RoundTripper, serverURL, cacheURL *url.URL, front string, p []byte) (io.ReadCloser, error) {
	// Build request URL: <serverURL>/<version+cacheBuster>/<base64-payload>
	u := serverURL.ResolveReference(&url.URL{
		// Use strings.Join (not path.Join) to retain a trailing slash when
		// p is empty.
		Path: strings.Join([]string{
			// "0" is the client–server protocol version indicator.
			"0" + base64.RawURLEncoding.EncodeToString(cacheBreaker()),
			base64.RawURLEncoding.EncodeToString(p),
		}, "/"),
	})

	// Route through an AMP cache if one was specified.
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

	// Domain fronting: replace the Host header while keeping the URL path.
	if front != "" {
		_, port, err := net.SplitHostPort(req.URL.Host)
		if err == nil {
			req.URL.Host = net.JoinHostPort(front, port)
		} else {
			req.URL.Host = front
		}
	}

	// Suppress the default Go user-agent so the AMP cache doesn't fingerprint
	// us differently from a browser.
	req.Header.Set("User-Agent", "")
	// NOTE: Do NOT set Accept-Encoding manually.
	// Go's http.Transport only auto-decompresses when it adds the header
	// itself. If we set it manually, we receive raw compressed bytes and
	// the AMP armor decoder fails with EOF.

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("server returned status %v", resp.Status)
	}
	if _, err := resp.Location(); err == nil {
		// The Google AMP Cache can return a "silent redirect" (status 200
		// + Location header + JS redirect in body).  We cannot extract
		// data from this response; treat it as a poll error.
		//
		// Example response headers:
		//   HTTP/2 200 OK
		//   Cache-Control: private
		//   Location: https://example.com/champa/...
		//   X-Silent-Redirect: true
		resp.Body.Close()
		return nil, fmt.Errorf("server returned a Location header (silent redirect)")
	}

	dec, err := armor.NewDecoder(bufio.NewReader(resp.Body))
	if err != nil {
		resp.Body.Close()
		return nil, err
	}

	// Return a ReadCloser that reads from the armor decoder (which reads
	// from the response body) but closes the actual response body.
	return &struct {
		io.Reader
		io.Closer
	}{
		Reader: dec,
		Closer: resp.Body,
	}, nil
}
