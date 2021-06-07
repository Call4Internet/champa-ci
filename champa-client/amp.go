package main

import (
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

// cacheBreaker returns a random byte slice of fixed length.
func cacheBreaker() []byte {
	buf := make([]byte, 12)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}
	return buf
}

func exchangeAMP(serverURL, cacheURL *url.URL, front string, p []byte) (io.ReadCloser, error) {
	// Append a cache buster and the encoded p to the path of serverURL.
	u := serverURL.ResolveReference(&url.URL{
		// Use strings.Join, rather than path.Join, to retain the
		// closing slash when p is empty.
		Path: strings.Join([]string{
			base64.RawURLEncoding.EncodeToString(cacheBreaker()),
			base64.RawURLEncoding.EncodeToString(p),
		}, "/"),
	})

	// Proxy through an AMP cache, if requested.
	if cacheURL != nil {
		var err error
		u, err = amp.CacheURL(u, cacheURL, "c")
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	// Do domain fronting, if requested.
	if front != "" {
		_, port, err := net.SplitHostPort(req.URL.Host)
		if err == nil {
			req.URL.Host = net.JoinHostPort(front, port)
		} else {
			req.URL.Host = front
		}
	}

	req.Header.Set("User-Agent", "") // Disable default "Go-http-client/1.1".

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("server returned status %v", resp.Status)
	}
	if loc, err := resp.Location(); err == nil {
		resp.Body.Close()
		return nil, fmt.Errorf("server returned a redirect (Location: %+q)", loc)
	}

	// The caller should read from the decoder (which reads from the
	// response body), but close the actual response body when done.
	return &struct {
		io.Reader
		io.Closer
	}{
		Reader: armor.NewDecoder(resp.Body),
		Closer: resp.Body,
	}, nil
}
