package router

import (
	"net"
	"testing"

	"github.com/IrineSistiana/mosproxy/internal/pool"
	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/miekg/dns"
)

func dohTestMsgs(t *testing.T, answers int) (*dnsmsg.Msg, *dnsmsg.Msg) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	q.SetEdns0(1232, false)
	r := new(dns.Msg)
	r.SetReply(q)
	for i := 0; i < answers; i++ {
		r.Answer = append(r.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.IPv4(10, 0, byte(i/256), byte(i%256)),
		})
	}
	unpack := func(m *dns.Msg) *dnsmsg.Msg {
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		out := dnsmsg.NewMsg()
		if err := out.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return unpack(q), unpack(r)
}

func TestDoHBodyIsABareMessageThatIsNeverTruncated(t *testing.T) {
	req, resp := dohTestMsgs(t, 150)
	defer dnsmsg.ReleaseMsg(req)
	defer dnsmsg.ReleaseMsg(resp)
	b := serverFinalRespHTTP(req, resp)
	defer pool.ReleaseBuf(b)
	got := new(dns.Msg)
	if err := got.Unpack(b); err != nil {
		t.Fatalf("DoH body does not parse as a DNS message: %v", err)
	}
	if got.Truncated || len(got.Answer) != 150 {
		t.Fatalf("DoH answer truncated: TC=%v answers=%d of 150 (%d bytes)", got.Truncated, len(got.Answer), len(b))
	}
}

func TestDoHRefusedBodyHasNoLengthPrefix(t *testing.T) {
	req, _ := dohTestMsgs(t, 0)
	defer dnsmsg.ReleaseMsg(req)
	resp := makeEmptyRespM(req, dnsmsg.RCodeRefused)
	defer dnsmsg.ReleaseMsg(resp)
	b := serverFinalRespHTTP(req, resp)
	defer pool.ReleaseBuf(b)
	got := new(dns.Msg)
	if err := got.Unpack(b); err != nil {
		t.Fatalf("refused DoH body does not parse: %v", err)
	}
	if got.Rcode != dns.RcodeRefused || !got.Response {
		t.Fatalf("got rcode %d response=%v, want REFUSED response", got.Rcode, got.Response)
	}
}

func TestDoHAcceptHeader(t *testing.T) {
	for v, want := range map[string]bool{
		"":                        true,
		"application/dns-message": true,
		"*/*":                     true,
		"application/*":           true,
		"text/html, application/dns-message;q=0.9": true,
		"Application/DNS-Message":                  true,
		"application/dns-message;q=0":              false,
		"application/dns-json":                     false,
		"text/html":                                false,
	} {
		if got := acceptsDNSMessage(v); got != want {
			t.Errorf("acceptsDNSMessage(%q) = %v, want %v", v, got, want)
		}
	}
}
