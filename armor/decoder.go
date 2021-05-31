package armor

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/net/html"
)

func isASCIIWhitespace(b byte) bool {
	switch b {
	// https://infra.spec.whatwg.org/#ascii-whitespace
	case '\x09', '\x0a', '\x0c', '\x0d', '\x20':
		return true
	default:
		return false
	}
}

func splitASCIIWhitespace(data []byte, atEOF bool) (advance int, token []byte, err error) {
	var i, j int
	// Skip initial whitespace.
	for i = 0; i < len(data); i++ {
		if !isASCIIWhitespace(data[i]) {
			break
		}
	}
	// Look for next whitespace.
	for j = i; j < len(data); j++ {
		if isASCIIWhitespace(data[j]) {
			return j + 1, data[i:j], nil
		}
	}
	// We reached the end of data without finding more whitespace. Only
	// consider it a token if we are at EOF.
	if atEOF && i < j {
		return j, data[i:j], nil
	}
	// Otherwise, request more data.
	return i, nil, nil
}

func decodeToWriter(w io.Writer, r io.Reader) (int64, error) {
	tokenizer := html.NewTokenizer(r)
	// Set a memory limit on token sizes, otherwise the tokenizer will
	// buffer text indefinitely if it is not broken up by other token types.
	tokenizer.SetMaxBuf(elementSizeLimit)
	active := false
	total := int64(0)
	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			err := tokenizer.Err()
			if err == io.EOF {
				err = nil
			}
			if err == nil && active {
				return total, fmt.Errorf("missing </pre> tag")
			}
			return total, err
		case html.TextToken:
			if active {
				// Re-join the separate chunks of base64 and
				// feed them to the decoder.
				// https://infra.spec.whatwg.org/#forgiving-base64
				scanner := bufio.NewScanner(bytes.NewReader(tokenizer.Text()))
				scanner.Split(splitASCIIWhitespace)
				for scanner.Scan() {
					n, err := w.Write(scanner.Bytes())
					total += int64(n)
					if err != nil {
						return total, err
					}
				}
				if err := scanner.Err(); err != nil {
					return total, err
				}
			}
		case html.StartTagToken:
			tn, _ := tokenizer.TagName()
			if string(tn) == "pre" {
				if active {
					// nesting not allowed
					return total, fmt.Errorf("unexpected %s", tokenizer.Token())
				}
				active = true
			}
		case html.EndTagToken:
			tn, _ := tokenizer.TagName()
			if string(tn) == "pre" {
				if !active {
					// stray end tag
					return total, fmt.Errorf("unexpected %s", tokenizer.Token())
				}
				active = false
			}
		}
	}
}

// NewDecoder returns a new AMP armor decoder.
func NewDecoder(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	dec := base64.NewDecoder(base64.StdEncoding, pr)
	go func() {
		_, err := decodeToWriter(pw, r)
		pw.CloseWithError(err)
	}()
	return dec
}
