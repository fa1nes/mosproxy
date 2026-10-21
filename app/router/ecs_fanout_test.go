package router

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// scopedUpstream answers every A query with an address derived from the ECS
// subnet it was sent, and echoes that subnet back with the given scope.
type scopedUpstream struct {
	scope uint8
	mu    sync.Mutex
	seen  []netip.Prefix
}

func (u *scopedUpstream) Exchange(_ context.Context, q *QueryCtx, m *dnsmsg.Msg) error {
	ecs := findECS(m)
	u.mu.Lock()
	u.seen = append(u.seen, ecs)
	u.mu.Unlock()

	resp := new(dns.Msg)
	resp.SetQuestion("cdn.example.", dns.TypeA)
	resp.Response = true
	ip := net.IPv4(10, 0, 0, 1)
	if ecs.IsValid() {
		ip = net.IP(ecs.Addr().AsSlice())
	}
	resp.Answer = append(resp.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 600},
		A:   ip,
	})
	resp.SetEdns0(1232, false)
	opt := resp.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
		Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: uint8(ecs.Bits()),
		SourceScope: u.scope, Address: ecs.Addr().AsSlice(),
	})
	b, err := resp.Pack()
	if err != nil {
		return err
	}
	msg, err := dnsmsg.UnpackMsg(b)
	if err != nil {
		return err
	}
	q.SetResp(msg)
	return nil
}

func (u *scopedUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

func newFanoutRouter(t *testing.T, fanout string) *Router {
	t.Helper()
	logger := zerolog.Nop()
	r := &Router{
		opt:        &Config{Cache: CacheConfig{MemSize: 1 << 20, PrefetchThreshold: 0.25}},
		ctx:        context.Background(),
		logger:     &logger,
		metricsReg: prometheus.NewRegistry(),
		prefetchSf: newPrefetchCtl(),

		prefetchTotal:  prometheus.NewCounter(prometheus.CounterOpts{Name: "p"}),
		ecsFanoutTotal: prometheus.NewCounter(prometheus.CounterOpts{Name: "f"}),
	}
	c, err := r.initCache(&r.opt.Cache)
	if err != nil {
		t.Fatal(err)
	}
	r.cache = c
	fp := filepath.Join(t.TempDir(), "fanout.txt")
	if err := os.WriteFile(fp, []byte(fanout), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ecsFanout, err = r.loadMark2Prefix([]string{fp}, "ecs_fanout")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func liveQuery(t *testing.T, r *Router, u Upstream, zone string, subnet string) *QueryCtx {
	t.Helper()
	q := NewQueryCtx()
	if err := dnsmsg.ParseName(&q.Question.Name, "cdn.example."); err != nil {
		t.Fatal(err)
	}
	q.Question.Type = dnsmsg.TypeA
	q.Question.Class = dnsmsg.ClassINET
	q.ECSZone = zone
	q.ECS2Upstream = netip.MustParsePrefix(subnet)
	if err := r.forward(context.Background(), q, u); err != nil {
		t.Fatal(err)
	}
	return q
}

func cachedFor(r *Router, zone string) *dnsmsg.Msg {
	q := NewQueryCtx()
	defer ReleaseQueryCtx(q)
	_ = dnsmsg.ParseName(&q.Question.Name, "cdn.example.")
	q.Question.Type = dnsmsg.TypeA
	q.Question.Class = dnsmsg.ClassINET
	q.ECSZone = zone
	resp, _ := r.cache.Get(context.Background(), r.appendCacheKey(nil, q, "lb"))
	return resp
}

func waitCalls(u *scopedUpstream, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if u.count() >= want {
			time.Sleep(20 * time.Millisecond) // let Store land
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

const fanoutFile = "广东电信,113.64.1.0/24\n广东移动,120.230.1.0/24\n广东联通,112.90.1.0/24\n"

func TestFanoutWarmsOtherZonesWithTheirOwnSubnet(t *testing.T) {
	r := newFanoutRouter(t, fanoutFile)
	u := &scopedUpstream{scope: 24}
	q := liveQuery(t, r, u, "广东电信", "113.64.200.0/24")
	defer ReleaseQueryCtx(q)
	if q.ECSScope != 24 {
		t.Fatalf("scope = %d, want 24", q.ECSScope)
	}
	r.fanoutECS(q, u, "lb")
	if !waitCalls(u, 3) {
		t.Fatalf("expected 2 fanout queries, upstream saw %d in total", u.count())
	}
	for zone, want := range map[string]string{"广东移动": "120.230.1.0", "广东联通": "112.90.1.0"} {
		resp := cachedFor(r, zone)
		if resp == nil || len(resp.Answers) != 1 {
			t.Fatalf("%s: no cached answer after fanout", zone)
		}
		got := resp.Answers[0].(*dnsmsg.A).A
		if netip.AddrFrom4(got).String() != want {
			t.Fatalf("%s: cached %s, want the answer for its own subnet %s", zone, got, want)
		}
		dnsmsg.ReleaseMsg(resp)
	}
	if cachedFor(r, "广东电信") != nil {
		t.Fatal("fanout must not write the asking zone's own entry")
	}
}

func TestFanoutSkipsAnswersThatAreNotSubnetSpecific(t *testing.T) {
	r := newFanoutRouter(t, fanoutFile)
	u := &scopedUpstream{scope: 0}
	q := liveQuery(t, r, u, "广东电信", "113.64.200.0/24")
	defer ReleaseQueryCtx(q)
	r.fanoutECS(q, u, "lb")
	time.Sleep(100 * time.Millisecond)
	if n := u.count(); n != 1 {
		t.Fatalf("scope 0 answer fanned out: upstream saw %d queries", n)
	}
}

func TestFanoutIgnoresZonesNotListed(t *testing.T) {
	r := newFanoutRouter(t, fanoutFile)
	u := &scopedUpstream{scope: 24}
	q := liveQuery(t, r, u, "北京联通", "202.106.0.0/24")
	defer ReleaseQueryCtx(q)
	r.fanoutECS(q, u, "lb")
	time.Sleep(100 * time.Millisecond)
	if n := u.count(); n != 1 {
		t.Fatalf("unlisted zone fanned out: upstream saw %d queries", n)
	}
}

func TestFanoutLeavesFreshEntriesAlone(t *testing.T) {
	r := newFanoutRouter(t, fanoutFile)
	u := &scopedUpstream{scope: 24}
	q := liveQuery(t, r, u, "广东电信", "113.64.200.0/24")
	r.fanoutECS(q, u, "lb")
	ReleaseQueryCtx(q)
	if !waitCalls(u, 3) {
		t.Fatal("first fanout did not run")
	}
	q = liveQuery(t, r, u, "广东电信", "113.64.200.0/24")
	defer ReleaseQueryCtx(q)
	r.fanoutECS(q, u, "lb")
	time.Sleep(100 * time.Millisecond)
	if n := u.count(); n != 4 {
		t.Fatalf("fresh sibling entries were re-queried: upstream saw %d, want 4", n)
	}
}
