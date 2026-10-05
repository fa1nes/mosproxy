package dnsmsg

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func TestCopiedNameOwnsItsLabels(t *testing.T) {
	src := NewName()
	require.NoError(t, src.Parse("www.example.com"))
	var dst Name
	dst.CopyFrom(*src)
	ReleaseName(src)
	require.Equal(t, "www.example.com", string(dst.AppendReadableTo(nil)))

	reused := NewName()
	require.NoError(t, reused.Parse("other.test"))
	defer ReleaseName(reused)
	require.Equal(t, "www.example.com", string(dst.AppendReadableTo(nil)))
}

func TestCopyingANameOntoItselfKeepsIt(t *testing.T) {
	n := NewName()
	defer ReleaseName(n)
	require.NoError(t, n.Parse("www.example.com"))
	n.CopyFrom(*n)
	require.Equal(t, "www.example.com", string(n.AppendReadableTo(nil)))
}

func TestMalformedWireNameIsAnErrorNotAPanic(t *testing.T) {
	for _, raw := range [][]byte{{63, 'a'}, {3, 'a', 'b'}, {0xc0, 0x0c}, {3, 'a', 'b', 'c'}} {
		n := NewName()
		require.Error(t, ParseNameRaw(n, raw), "%v", raw)
		ReleaseName(n)
		var dst Name
		require.NotPanics(t, func() { dst.CopyFrom(Name{b: raw}) }, "%v", raw)
	}
}

func TestParse(t *testing.T) {
	r := require.New(t)

	testFn := func(s string, expect error) {
		n := NewName()
		err := n.Parse(s)
		if err != nil {
			r.ErrorIs(err, expect)
			return
		}

		out, _, err := dns.UnpackDomainName(n.Data(), 0)
		r.NoError(err)
		r.Equal(dns.Fqdn(s), dns.Fqdn(out))
	}

	// root
	testFn("", nil)
	testFn(".", nil)

	for i := 0; i < 100; i++ {
		s := fmt.Sprintf("%x.%x.%x", rand.Int31(), rand.Int31(), rand.Int31())
		testFn(s, nil)
	}

	testFn("aaa.aa\\255.\\000.\\\\.\\..aaa.aaa", nil)
	testFn(".a", errZeroSegLen)
	testFn("a.b.c..d", errZeroSegLen)
	testFn("a.b."+strings.Repeat("c", 63)+".d", nil)
	testFn("a.b."+strings.Repeat("c", 64)+".d", errSegTooLong)
	testFn(strings.Repeat("c.", 127), nil)
	testFn(strings.Repeat("c.", 128), errNameTooLong)
}

func TestName_AppendReadableTo(t *testing.T) {
	r := require.New(t)
	testFn := func(s, w string) {
		n := NewName()
		err := n.Parse(s)
		r.NoError(err)

		got := n.AppendReadableTo(nil)
		r.Equal(w, string(got))
	}
	testFn("1.2.3", "1.2.3")
	testFn("1.2.3.", "1.2.3")
	testFn("aaa.aa\\123.\\000.\\\\.\\..aaa.aaa", "aaa.aa\\123.\\000.\\\\.\\..aaa.aaa")
	testFn("a.bb.ccc.dddd", "a.bb.ccc.dddd")
	testFn("", ".")
}
