package amp

import (
	"bytes"
	"testing"

	"golang.org/x/net/idna"
)

func TestDomainPrefixBasic(t *testing.T) {
	// Tests expecting no error.
	for _, test := range []struct {
		domain, expected string
	}{
		{"", ""},
		{"xn--", ""},
		{"...", "---"},

		// Should not apply mappings such as case folding and
		// normalization.
		{"b\u00fccher.de", "xn--bcher-de-65a"},
		{"B\u00fccher.de", "xn--Bcher-de-65a"},
		{"bu\u0308cher.de", "xn--bucher-de-hkf"},

		// Check some that differ between IDNA 2003 and IDNA 2008.
		// https://unicode.org/reports/tr46/#Deviations
		// https://util.unicode.org/UnicodeJsps/idna.jsp
		{"faß.de", "xn--fa-de-mqa"},
		{"βόλοσ.com", "xn---com-4ld8c2a6a8e"},

		// Lengths of 63 and 64. 64 is too long for a DNS label, but
		// domainPrefixBasic is not expected to check for that.
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},

		// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#basic-algorithm
		{"example.com", "example-com"},
		{"foo.example.com", "foo-example-com"},
		{"foo-example.com", "foo--example-com"},
		{"xn--57hw060o.com", "xn---com-p33b41770a"},
		{"\u26a1\U0001f60a.com", "xn---com-p33b41770a"},
		{"en-us.example.com", "0-en--us-example-com-0"},
	} {
		output, err := domainPrefixBasic(test.domain)
		if err != nil || output != test.expected {
			t.Errorf("%+q → (%+q, %v), expected (%+q, %v)",
				test.domain, output, err, test.expected, nil)
		}
	}

	// Tests expecting an error.
	for _, domain := range []string{
		"xn---",
	} {
		output, err := domainPrefixBasic(domain)
		if err == nil || output != "" {
			t.Errorf("%+q → (%+q, %v), expected (%+q, non-nil)",
				domain, output, err, "")
		}
	}
}

func TestDomainPrefixFallback(t *testing.T) {
	for _, test := range []struct {
		domain, expected string
	}{
		{
			"",
			"4oymiquy7qobjgx36tejs35zeqt24qpemsnzgtfeswmrw6csxbkq",
		},
		{
			"example.com",
			"un42n5xov642kxrxrqiyanhcoupgql5lt4wtbkyt2ijflbwodfdq",
		},

		// These checked against the output of
		// https://github.com/ampproject/amp-toolbox/tree/84cb3057e5f6c54d64369ddd285db1cb36237ee8/packages/cache-url,
		// using the widget at
		// https://amp.dev/documentation/guides-and-tutorials/learn/amp-caches-and-cors/amp-cache-urls/#url-format.
		{
			"000000000000000000000000000000000000000000000000000000000000.com",
			"stejanx4hsijaoj4secyecy4nvqodk56kw72whwcmvdbtucibf5a",
		},
		{
			"00000000000000000000000000000000000000000000000000000000000a.com",
			"jdcvbsorpnc3hcjrhst56nfm6ymdpovlawdbm2efyxpvlt4cpbya",
		},
		{
			"00000000000000000000000000000000000000000000000000000000000\u03bb.com",
			"qhzqeumjkfpcpuic3vqruyjswcr7y7gcm3crqyhhywvn3xrhchfa",
		},
	} {
		output := domainPrefixFallback(test.domain)
		if output != test.expected {
			t.Errorf("%+q → %+q, expected %+q",
				test.domain, output, test.expected)
		}
	}
}

// Checks that domainPrefix chooses domainPrefixBasic or domainPrefixFallback as
// appropriate; i.e., always returns string that is a valid DNS label and is
// IDNA-decodable.
func TestDomainPrefix(t *testing.T) {
	// A validating IDNA profile, which checks label length and that the
	// label contains only certain ASCII characters. It does not do the
	// ValidateLabels check, because that depends on the input having
	// certain properties.
	profile := idna.New(
		idna.VerifyDNSLength(true),
		idna.StrictDomainName(true),
	)
	for _, domain := range []string{
		"example.com",
		"\u0314example.com",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",  // 63 bytes
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 64 bytes
		"xn--57hw060o.com",
		"a b c",
	} {
		output := domainPrefix(domain)
		if bytes.IndexByte([]byte(output), '.') != -1 {
			t.Errorf("%+q → %+q contains a dot", domain, output)
		}
		_, err := profile.ToUnicode(output)
		if err != nil {
			t.Errorf("%+q → error %v", domain, err)
		}
	}
}
