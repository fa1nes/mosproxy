package dnsmsg

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/IrineSistiana/mosproxy/internal/pool"
)

// Real wire-format RDATA captured from the production resolver with
// `dig +unknownformat crypto.cloudflare.com HTTPS`. Hand-built fixtures are not good
// enough here: the whole point is that this code has to survive what Cloudflare
// actually sends, including the exact SvcParam ordering and the 71-byte ech blob.
//
//	0001                    SvcPriority 1
//	00                      TargetName "." (ServiceMode, no alias)
//	0001 0003 026832        alpn = h2
//	0004 0008 A29F874F ...  ipv4hint = 162.159.135.79, 162.159.136.79
//	0005 0047 0045FE0D...   ech (71 bytes) <- the one to remove
//	0006 0020 26064700...   ipv6hint
const echRdataHex = "0001000001000302683200040008A29F874FA29F884F000500470045" +
	"FE0D0041D800200020528FBEB9D5F1AAE4074BEC6AEFC8E62E54E16F" +
	"A68DBD9B532FF2A4E082A44D240004000100010012636C6F7564666C" +
	"6172652D6563682E636F6D0000000600202606470000070000000000" +
	"00A29F874F260647000007000000000000A29F884F"

// Same capture for cloudflare.com, which carries no ech — the majority case.
const noEchRdataHex = "0001000001000602683302683200040008681084E5681085E5000600" +
	"20260647000000000000000000681084E52606470000000000000000" +
	"00681085E5"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return b
}

// paramKeys lists the SvcParamKeys present in a ServiceMode RDATA, so a test can assert
// on structure instead of on an opaque byte blob.
func paramKeys(t *testing.T, rdata []byte) []uint16 {
	t.Helper()
	off := 2
	for rdata[off] != 0 {
		off += 1 + int(rdata[off])
	}
	off++
	var keys []uint16
	for p := off; p < len(rdata); {
		keys = append(keys, binary.BigEndian.Uint16(rdata[p:]))
		p += 4 + int(binary.BigEndian.Uint16(rdata[p+2:]))
	}
	return keys
}

func TestStripECHFromRdata_removesOnlyECH(t *testing.T) {
	rdata := mustHex(t, echRdataHex)
	if got := len(rdata); got != 133 {
		t.Fatalf("fixture length = %d, want 133", got)
	}

	n, ok := stripECHFromRdata(rdata)
	if !ok {
		t.Fatal("stripECHFromRdata reported no change on a record that carries ech")
	}
	// 133 - (4 byte param header + 71 byte value) = 58
	if n != 58 {
		t.Errorf("new length = %d, want 58", n)
	}

	got := paramKeys(t, rdata[:n])
	want := []uint16{1, 4, 6} // alpn, ipv4hint, ipv6hint — ech (5) gone, order preserved
	if len(got) != len(want) {
		t.Fatalf("params = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("params = %v, want %v", got, want)
		}
	}

	// alpn and the address hints must survive untouched: dropping them would cost HTTP/3
	// and Happy Eyeballs, which is exactly what this implementation avoids.
	if !bytes.Contains(rdata[:n], mustHex(t, "0001000302683200")[:6]) {
		t.Error("alpn parameter was damaged")
	}
	if !bytes.Contains(rdata[:n], mustHex(t, "A29F874FA29F884F")) {
		t.Error("ipv4hint payload was damaged")
	}
	if bytes.Contains(rdata[:n], mustHex(t, "FE0D0041D8")) {
		t.Error("ech payload still present after strip")
	}
}

func TestStripECHFromRdata_noECHIsUntouched(t *testing.T) {
	rdata := mustHex(t, noEchRdataHex)
	original := bytes.Clone(rdata)

	if _, ok := stripECHFromRdata(rdata); ok {
		t.Error("reported a change on a record without ech")
	}
	if !bytes.Equal(rdata, original) {
		t.Error("record without ech was modified")
	}
}

func TestStripECHFromRdata_malformedInputIsRejected(t *testing.T) {
	// Every one of these must be refused rather than panic: RDATA arrives from the
	// network, and a resolver that crashes on a hostile HTTPS record is a denial of
	// service on the whole stack.
	cases := map[string]string{
		"empty":               "",
		"truncated priority":  "00",
		"alias mode":          "0000" + "00", // priority 0 has no SvcParams
		"target runs off end": "0001" + "05616263", // label claims 5 bytes, 3 present
		"compression pointer": "0001" + "C00C",
		"param header cut":    "0001" + "00" + "000500",
		"param value cut":     "0001" + "00" + "00050047" + "DEADBEEF",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			rdata := mustHex(t, h)
			original := bytes.Clone(rdata)
			if _, ok := stripECHFromRdata(rdata); ok {
				t.Error("malformed RDATA was accepted")
			}
			if !bytes.Equal(rdata, original) {
				t.Error("malformed RDATA was partially rewritten before rejection")
			}
		})
	}
}

func newHTTPSRecord(t *testing.T, rdataHex string) *RawResource {
	t.Helper()
	rr := NewRaw()
	rr.Hdr().Type = TypeHTTPS
	rr.Hdr().Class = ClassINET
	rr.Data = pool.CopyBuf(mustHex(t, rdataHex))
	rr.Hdr().Length = uint16(len(rr.Data))
	return rr
}

func TestStripECH_message(t *testing.T) {
	m := NewMsg()
	defer ReleaseMsg(m)
	m.Header.AuthenticData = true
	m.Answers = append(m.Answers, newHTTPSRecord(t, echRdataHex))

	// An RRSIG that covers HTTPS (type 65 in the first two RDATA bytes).
	sig := NewRaw()
	sig.Hdr().Type = TypeRRSIG
	sig.Data = pool.CopyBuf(mustHex(t, "0041"+"0D02"+"00000E10"))
	m.Answers = append(m.Answers, sig)

	// An RRSIG covering A (type 1) must be left alone.
	otherSig := NewRaw()
	otherSig.Hdr().Type = TypeRRSIG
	otherSig.Data = pool.CopyBuf(mustHex(t, "0001"+"0D02"+"00000E10"))
	m.Answers = append(m.Answers, otherSig)

	if !StripECH(m) {
		t.Fatal("StripECH reported no change")
	}
	if len(m.Answers) != 2 {
		t.Fatalf("answers = %d, want 2 (HTTPS + unrelated RRSIG)", len(m.Answers))
	}
	if m.Header.AuthenticData {
		t.Error("AD bit still set after rewriting a signed record")
	}
	for _, rr := range m.Answers {
		raw := rr.(*RawResource)
		if raw.Hdr().Type == TypeRRSIG && binary.BigEndian.Uint16(raw.Data[:2]) == 65 {
			t.Error("RRSIG covering HTTPS survived the strip")
		}
		if raw.Hdr().Type == TypeHTTPS {
			if int(raw.Hdr().Length) != len(raw.Data) {
				t.Errorf("header length %d out of sync with data %d",
					raw.Hdr().Length, len(raw.Data))
			}
		}
	}
}

func TestStripECH_noopKeepsADBit(t *testing.T) {
	m := NewMsg()
	defer ReleaseMsg(m)
	m.Header.AuthenticData = true
	m.Answers = append(m.Answers, newHTTPSRecord(t, noEchRdataHex))

	if StripECH(m) {
		t.Error("reported a change for a message without ech")
	}
	// Nothing was rewritten, so the validated flag must stay — clearing it here would
	// downgrade every DNSSEC-signed answer that merely happens to have an HTTPS record.
	if !m.Header.AuthenticData {
		t.Error("AD bit cleared even though nothing was modified")
	}
}
