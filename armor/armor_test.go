package armor

import (
	"crypto/rand"
	"io"
	"io/ioutil"
	"strings"
	"testing"
)

func decodeToString(src string) (string, error) {
	p, err := ioutil.ReadAll(NewDecoder(strings.NewReader(src)))
	return string(p), err
}

func TestDecoder(t *testing.T) {
	for _, test := range []struct {
		input          string
		expectedOutput string
		expectedErr    bool
	}{
		{`
aGVsbG8gd29ybGQK
blah blah blah
<pre>
aGVsbG8gd29ybGQK
</pre>
aGVsbG8gd29ybGQK
blah blah blah
`,
			"hello world\n",
			false,
		},
		{`
<pre>
QUJDREVG
R0hJSktM
TU5PUFFS
</pre>
junk
<pre>
U1RVVldY
WVowMTIz
NDU2Nzg5
Cg
=
</pre>
<pre>
=
</pre>
`,
			"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\n",
			false,
		},
		// no <pre> tags
		{`
aGVsbG8gd29ybGQK
blah blah blah
aGVsbG8gd29ybGQK
aGVsbG8gd29ybGQK
blah blah blah
`,
			"",
			false,
		},
		// empty <pre> tags
		{`
aGVsbG8gd29ybGQK
blah blah blah
<pre>   </pre>
aGVsbG8gd29ybGQK
aGVsbG8gd29ybGQK<pre></pre>
blah blah blah
`,
			"",
			false,
		},
		// other tags inside <pre>
		{
			"blah <pre>aGVsb<p>G8gd29</p>ybGQK</pre>",
			"hello world\n",
			false,
		},
		// HTML comment
		{
			"blah <!-- <pre>aGVsbG8gd29ybGQK</pre> -->",
			"",
			false,
		},
		// all kinds of ASCII whitespace
		{
			"blah <pre>\x09aG\x0aV\x0csb\x0dG8\x20gd29ybGQK</pre>",
			"hello world\n",
			false,
		},

		// bad padding
		{`
<pre>
QUJDREVG
R0hJSktM
TU5PUFFS
</pre>
junk
<pre>
U1RVVldY
WVowMTIz
NDU2Nzg5
Cg
=
</pre>
`,
			"",
			true,
		},
		/*
			// per-chunk base64
			// test disabled because Go stdlib handles this incorrectly:
			// https://github.com/golang/go/issues/31626
			{
				"<pre>QQ==</pre><pre>Qg==</pre>",
				"",
				true,
			},
		*/
		// missing </pre>
		{
			"blah <pre></pre><pre>aGVsbG8gd29ybGQK",
			"",
			true,
		},
		// nested <pre>
		{
			"blah <pre>aGVsb<pre>G8gd29</pre>ybGQK</pre>",
			"",
			true,
		},
	} {
		output, err := decodeToString(test.input)
		if test.expectedErr && err == nil {
			t.Errorf("%+q → (%+q, %v), expected error", test.input, output, err)
			continue
		}
		if !test.expectedErr && err != nil {
			t.Errorf("%+q → (%+q, %v), expected no error", test.input, output, err)
			continue
		}
		if !test.expectedErr && output != test.expectedOutput {
			t.Errorf("%+q → (%+q, %v), expected (%+q, %v)",
				test.input, output, err, test.expectedOutput, nil)
			continue
		}
	}
}

func roundtrip(s string) (string, error) {
	var encoded strings.Builder
	enc, err := NewEncoder(&encoded)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(enc, strings.NewReader(s))
	if err != nil {
		return "", err
	}
	err = enc.Close()
	if err != nil {
		return "", err
	}
	return decodeToString(encoded.String())
}

func TestRoundtrip(t *testing.T) {
	lengths := make([]int, 0)
	// Test short strings and lengths around elementSizeLimit thresholds.
	for i := 0; i < bytesPerChunk*2; i++ {
		lengths = append(lengths, i)
	}
	for i := -10; i < +10; i++ {
		lengths = append(lengths, elementSizeLimit+i)
		lengths = append(lengths, 2*elementSizeLimit+i)
	}
	for _, n := range lengths {
		buf := make([]byte, n)
		rand.Read(buf)
		input := string(buf)
		output, err := roundtrip(input)
		if err != nil {
			t.Errorf("length %d → error %v", n, err)
			continue
		}
		if output != input {
			t.Errorf("length %d → %+q", n, output)
			continue
		}
	}
}
