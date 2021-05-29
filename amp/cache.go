package amp

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"

	"golang.org/x/net/idna"
)

// domainPrefixBasic does the basic domain prefix conversion. Does not do any
// IDNA mapping, such as https://www.unicode.org/reports/tr46/.
//
// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#basic-algorithm
func domainPrefixBasic(domain string) (string, error) {
	// 1. Punycode Decode the publisher domain.
	prefix, err := idna.ToUnicode(domain)
	if err != nil {
		return "", err
	}

	// 2. Replace any "-" (hyphen) character in the output of step 1 with
	//    "--" (two hyphens).
	prefix = strings.Replace(prefix, "-", "--", -1)

	// 3. Replace any "." (dot) character in the output of step 2 with "-"
	//    (hyphen).
	prefix = strings.Replace(prefix, ".", "-", -1)

	// 4. If the output of step 3 has a "-" (hyphen) at both positions 3 and
	//    4, then to the output of step 3, add a prefix of "0-" and add a
	//    suffix of "-0".
	if len(prefix) >= 4 && prefix[2] == '-' && prefix[3] == '-' {
		prefix = "0-" + prefix + "-0"
	}

	// 5. Punycode Encode the output of step 3.
	return idna.ToASCII(prefix)
}

// Lower-case base32 without padding.
var fallbackBase32Encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// domainPrefixFallback does the fallback domain prefix conversion. The returned
// base32 domain uses lower-case letters.
//
// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#fallback-algorithm
func domainPrefixFallback(domain string) string {
	// The algorithm specification does not say what, exactly, we are to
	// take the SHA-256 of. domain is notionally an abstract Unicode
	// string, not a byte sequence. While
	// https://github.com/ampproject/amp-toolbox/blob/84cb3057e5f6c54d64369ddd285db1cb36237ee8/packages/cache-url/lib/AmpCurlUrlGenerator.js#L62
	// says "Take the SHA256 of the punycode view of the domain," in reality
	// it hashes the UTF-8 encoding of the domain, without Punycode:
	// https://github.com/ampproject/amp-toolbox/blob/84cb3057e5f6c54d64369ddd285db1cb36237ee8/packages/cache-url/lib/AmpCurlUrlGenerator.js#L141
	// https://github.com/ampproject/amp-toolbox/blob/84cb3057e5f6c54d64369ddd285db1cb36237ee8/packages/cache-url/lib/browser/Sha256.js#L24
	// We do the same here, hashing the raw bytes of domain, presumed to be
	// UTF-8.

	// 1. Hash the publisher's domain using SHA256.
	h := sha256.Sum256([]byte(domain))

	// 2. Base32 Escape the output of step 1.
	// 3. Remove the last 4 characters from the output of step 2, which are
	//    always "=" (equals) characters.
	return fallbackBase32Encoding.EncodeToString(h[:])
}

// domainPrefix computes the domain prefix of an AMP cache URL.
//
// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#domain-name-prefix
func domainPrefix(domain string) string {
	// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#combined-algorithm
	// 1. Run the Basic Algorithm. If the output is a valid DNS label,
	//    [append the Cache domain suffix and] return. Otherwise continue to
	//    step 2.
	prefix, err := domainPrefixBasic(domain)
	// "A domain prefix is not a valid DNS label if it is longer than 63
	// characters"
	if err == nil && len(prefix) <= 63 {
		return prefix
	}
	// 2. Run the Fallback Algorithm. [Append the Cache domain suffix and]
	//    return.
	return domainPrefixFallback(domain)
}
