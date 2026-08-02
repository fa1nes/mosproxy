package router

import (
	"context"
	"testing"

	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/rs/zerolog"
)

type fixedResponseTransport struct {
	rcode dnsmsg.RCode
	calls int
}

func (t *fixedResponseTransport) ExchangeContext(
	_ context.Context, _ *dnsmsg.Msg,
) (*dnsmsg.Msg, error) {
	t.calls++
	resp := dnsmsg.NewMsg()
	resp.RCode = t.rcode
	return resp, nil
}

func (t *fixedResponseTransport) Close() error { return nil }

func TestFallThroughRetriesServfailInSameQuery(t *testing.T) {
	logger := zerolog.Nop()
	r := &Router{opt: &Config{}, ctx: context.Background(), logger: &logger}
	failed := &fixedResponseTransport{rcode: dnsmsg.RCodeServerFailure}
	succeeded := &fixedResponseTransport{rcode: dnsmsg.RCodeSuccess}

	lb := &LoadBalancer{
		tag:         "fallback-test",
		logger:      &logger,
		fallThrough: true,
		e: []*lbBackend{
			{u: r.wrapUpstream("failed", failed, &logger, HealthCheckConfig{}), weight: 1},
			{u: r.wrapUpstream("succeeded", succeeded, &logger, HealthCheckConfig{}), weight: 1},
		},
	}
	lb.buildIdx()

	q := NewQueryCtx()
	defer ReleaseQueryCtx(q)
	m := dnsmsg.NewMsg()
	defer dnsmsg.ReleaseMsg(m)
	if err := lb.Exchange(context.Background(), q, m); err != nil {
		t.Fatal(err)
	}
	if failed.calls != 1 || succeeded.calls != 1 {
		t.Fatalf("expected both backends once, got failed=%d succeeded=%d",
			failed.calls, succeeded.calls)
	}
	if q.Resp() == nil || q.Resp().RCode != dnsmsg.RCodeSuccess {
		t.Fatal("fallback response was not selected")
	}
}
