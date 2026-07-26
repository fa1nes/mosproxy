package dnsmsg

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

// Pack a response that is guaranteed to exceed the size limit and check that
// the truncation is actually reported: the TC flag must be set, and the section
// counts must match what was really written (a lying count would make a strict
// parser reject the message).
func TestTruncationSetsTCAndFixesCounts(t *testing.T) {
	src := new(dns.Msg)
	src.SetQuestion("example.com.", dns.TypeA)
	src.Response = true
	for i := 0; i < 80; i++ {
		src.Answer = append(src.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.IPv4(10, 0, byte(i/256), byte(i%256)),
		})
	}
	wire, err := src.Pack()
	if err != nil {
		t.Fatalf("build source msg: %v", err)
	}

	m := NewMsg()
	defer ReleaseMsg(m)
	if err := m.Unpack(wire); err != nil {
		t.Fatalf("unpack source: %v", err)
	}
	orig := len(m.Answers)

	b, err := m.Pack(nil, false, 512)
	if err != nil {
		t.Fatalf("pack with limit: %v", err)
	}
	if len(b) > 512 {
		t.Fatalf("packed %d bytes, exceeds the 512 byte limit", len(b))
	}

	got := new(dns.Msg)
	if err := got.Unpack(b); err != nil {
		t.Fatalf("packed message does not parse, section counts likely disagree with the body: %v", err)
	}
	if !got.Truncated {
		t.Errorf("records were dropped (%d -> %d) but the TC flag was not set", orig, len(got.Answer))
	}
	if len(got.Answer) == orig {
		t.Errorf("ANCOUNT still claims all %d answers are present", orig)
	}
	t.Logf("%d answers -> %d written, TC=%v, %d bytes", orig, len(got.Answer), got.Truncated, len(b))
}
