package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/rs/zerolog"
)

type slowZoneTransport struct {
	pingOK bool
	pings  atomic.Int32
}

func (t *slowZoneTransport) ExchangeContext(_ context.Context, m *dnsmsg.Msg) (*dnsmsg.Msg, error) {
	if len(m.Questions) == 1 && m.Questions[0].Type == dnsmsg.TypeNS {
		t.pings.Add(1)
		if t.pingOK {
			return dnsmsg.NewMsg(), nil
		}
	}
	return nil, context.DeadlineExceeded
}

func (t *slowZoneTransport) Close() error { return nil }

func failQueries(t *testing.T, uw *UpstreamWrapper, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		q := NewQueryCtx()
		m := dnsmsg.NewMsg()
		if err := uw.Exchange(context.Background(), q, m); err == nil {
			t.Fatal("query should fail")
		}
		dnsmsg.ReleaseMsg(m)
		ReleaseQueryCtx(q)
	}
}

func waitConfirm(t *testing.T, uw *UpstreamWrapper) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		uw.hcLock.Lock()
		busy := uw.hcConfirming
		uw.hcLock.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("confirmation ping did not finish")
}

func TestQueryTimeoutsDoNotOfflineAnUpstreamThatAnswersPing(t *testing.T) {
	logger := zerolog.Nop()
	r := &Router{opt: &Config{}, ctx: context.Background(), logger: &logger}
	tr := &slowZoneTransport{pingOK: true}
	uw := r.wrapUpstream("local", tr, &logger, HealthCheckConfig{MaxFails: 5, PingInterval: 30})
	defer uw.close()

	failQueries(t, uw, 6)
	waitConfirm(t, uw)
	if uw.HcOffline() {
		t.Fatal("upstream that answers ping was marked offline by slow queries")
	}
	if got := tr.pings.Load(); got != 1 {
		t.Fatalf("expected one confirmation ping, got %d", got)
	}

	failQueries(t, uw, 4)
	waitConfirm(t, uw)
	if got := tr.pings.Load(); got != 1 {
		t.Fatalf("failure counter should restart after a good ping, got %d pings", got)
	}
}

func TestUpstreamThatFailsPingGoesOffline(t *testing.T) {
	logger := zerolog.Nop()
	r := &Router{opt: &Config{}, ctx: context.Background(), logger: &logger}
	tr := &slowZoneTransport{pingOK: false}
	uw := r.wrapUpstream("dead", tr, &logger, HealthCheckConfig{MaxFails: 5, PingInterval: 30})
	defer uw.close()

	failQueries(t, uw, 5)
	waitConfirm(t, uw)
	if !uw.HcOffline() {
		t.Fatal("upstream that fails ping should be offline")
	}
	q := NewQueryCtx()
	defer ReleaseQueryCtx(q)
	m := dnsmsg.NewMsg()
	defer dnsmsg.ReleaseMsg(m)
	if err := uw.Exchange(context.Background(), q, m); !errors.Is(err, ErrUpstreamOffline) {
		t.Fatalf("offline upstream should be skipped, got %v", err)
	}
}
