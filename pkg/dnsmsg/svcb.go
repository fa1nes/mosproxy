package dnsmsg

import "encoding/binary"

// SVCB and HTTPS resource records (RFC 9460).
//
// These types are not modelled as dedicated Resource implementations: unpackResource
// falls through to RawResource for them, which keeps the wire-format RDATA intact in
// RawResource.Data. That is exactly what StripECH needs, so the parameter surgery is
// done on the raw bytes instead of introducing a full SVCB type.
const (
	TypeSVCB  Type = 64
	TypeHTTPS Type = 65
	TypeRRSIG Type = 46
)

// SvcParamKey values, RFC 9460 section 14.3.2.
const svcParamKeyECH uint16 = 5

// StripECH removes the "ech" SvcParam from every SVCB/HTTPS record in the message and
// drops the RRSIGs that covered them. It reports whether anything was changed.
//
// Why this exists: Cloudflare is rolling out Encrypted Client Hello. A client that can
// read the ech SvcParam encrypts the real SNI and puts a cover name
// ("cloudflare-ech.com") in the outer ClientHello. Every proxy that routes by domain
// then sees the cover name instead of the real host, so domain-based routing rules stop
// matching and the traffic silently falls through to the default outbound. Removing the
// key makes the client fall back to plain SNI, which is what the routing layer needs.
//
// The rest of the record is deliberately preserved. Dropping the whole HTTPS RR would
// also take away alpn="h3" (no more HTTP/3) and the ipv4hint/ipv6hint used for Happy
// Eyeballs, which costs real performance for no extra benefit.
func StripECH(m *Msg) bool {
	changed := false
	for _, section := range [][]Resource{m.Answers, m.Authorities, m.Additionals} {
		for _, rr := range section {
			raw, ok := rr.(*RawResource)
			if !ok {
				continue
			}
			if t := raw.Hdr().Type; t != TypeHTTPS && t != TypeSVCB {
				continue
			}
			n, ok := stripECHFromRdata(raw.Data)
			if !ok {
				continue
			}
			// Reslice instead of reassigning: Data comes from the buffer pool, and
			// appending into a fresh slice would drop the pooled buffer on the floor
			// without ever releasing it.
			raw.Data = raw.Data[:n]
			raw.Hdr().Length = uint16(n)
			changed = true
		}
	}
	if changed {
		dropSVCBSignatures(m)
		// The answer no longer matches what the zone signed, so keeping AD set would be
		// claiming "DNSSEC-validated" for bytes we just rewrote. Clients that trust AD
		// deserve an honest answer more than they deserve a green flag.
		m.Header.AuthenticData = false
	}
	return changed
}

// stripECHFromRdata rewrites rdata in place without its ech parameter and returns the
// new length. ok is false when the record has no ech key or cannot be parsed; in that
// case rdata is left completely untouched — the common case, since most HTTPS records
// carry no ech at all.
func stripECHFromRdata(rdata []byte) (newLen int, ok bool) {
	// SvcPriority(2) + TargetName + SvcParams
	if len(rdata) < 3 {
		return 0, false
	}
	// AliasMode (priority 0) carries no SvcParams at all.
	if binary.BigEndian.Uint16(rdata[:2]) == 0 {
		return 0, false
	}

	off := 2
	// TargetName. RFC 9460 section 2.2 forbids name compression here, so this is always
	// a plain label sequence terminated by a zero octet. A compression pointer means the
	// record is malformed (or from a broken server); bail out rather than guess.
	for {
		if off >= len(rdata) {
			return 0, false
		}
		l := int(rdata[off])
		if l == 0 {
			off++
			break
		}
		if l&0xC0 != 0 {
			return 0, false
		}
		off += 1 + l
		if off > len(rdata) {
			return 0, false
		}
	}

	// First pass: validate the whole parameter list and look for ech. Doing this before
	// touching anything means a malformed record is rejected without having been half
	// rewritten, and a well-formed record without ech costs one scan and no writes.
	paramsStart := off
	found := false
	for p := paramsStart; p < len(rdata); {
		if p+4 > len(rdata) {
			return 0, false
		}
		key := binary.BigEndian.Uint16(rdata[p:])
		vLen := int(binary.BigEndian.Uint16(rdata[p+2:]))
		if p+4+vLen > len(rdata) {
			return 0, false
		}
		if key == svcParamKeyECH {
			found = true
		}
		p += 4 + vLen
	}
	if !found {
		return 0, false
	}

	// Second pass: compact in place, dropping ech. The write cursor never overtakes the
	// read cursor (they advance together for kept params, and only the reader moves for
	// the dropped one), so copy always shifts data towards the front and never clobbers
	// a parameter that has not been read yet. SvcParams stay in ascending key order
	// because removing one entry from a sorted list keeps it sorted.
	w := paramsStart
	for p := paramsStart; p < len(rdata); {
		vLen := int(binary.BigEndian.Uint16(rdata[p+2:]))
		if binary.BigEndian.Uint16(rdata[p:]) != svcParamKeyECH {
			if w != p {
				copy(rdata[w:], rdata[p:p+4+vLen])
			}
			w += 4 + vLen
		}
		p += 4 + vLen
	}
	return w, true
}

// dropSVCBSignatures removes RRSIGs covering SVCB/HTTPS. Leaving them in place would be
// worse than having no signature at all: a validating client would fetch the DNSKEY,
// check the rewritten record against the original signature, fail, and treat the whole
// response as an attack — turning a routing fix into an outage.
func dropSVCBSignatures(m *Msg) {
	filter := func(section []Resource) []Resource {
		kept := section[:0]
		for _, rr := range section {
			if coversSVCB(rr) {
				ReleaseResource(rr)
				continue
			}
			kept = append(kept, rr)
		}
		return kept
	}
	m.Answers = filter(m.Answers)
	m.Authorities = filter(m.Authorities)
	m.Additionals = filter(m.Additionals)
}

func coversSVCB(rr Resource) bool {
	if rr.Hdr().Type != TypeRRSIG {
		return false
	}
	raw, ok := rr.(*RawResource)
	if !ok || len(raw.Data) < 2 {
		return false
	}
	// RRSIG RDATA starts with the covered type, RFC 4034 section 3.1.
	covered := Type(binary.BigEndian.Uint16(raw.Data[:2]))
	return covered == TypeHTTPS || covered == TypeSVCB
}
