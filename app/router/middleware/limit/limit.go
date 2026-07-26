package testmw

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/IrineSistiana/mosproxy/app/router"
	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

type Args struct {
	Concurrent int32 `yaml:"concurrent"`
	QPS        int   `yaml:"qps"`
}

func init() {
	router.RegMiddleware("limit", func(ctx router.PluginCtx, args map[string]any, next router.Middleware) (router.Middleware, error) {
		a := Args{}
		err := router.WakeDecode(&a, args, "yaml")
		if err != nil {
			return nil, fmt.Errorf("invalid args, %w", err)
		}

		h := &Limit{
			ctx:  ctx,
			next: next,
			args: a,

			rejectedCcTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "rejected_cc_total",
				Help: "The total number of queries rejected because the concurrent limit",
			}),
			rejectedQpsTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "rejected_qps_total",
				Help: "The total number of queries rejected because the qps limit",
			}),
		}

		if a.QPS > 0 {
			h.rate = rate.NewLimiter(rate.Limit(a.QPS), (a.QPS>>2)+1)
		}
		err = router.RegMetrics(ctx.R.GetMetricsReg(), h.rejectedCcTotal, h.rejectedQpsTotal)
		if err != nil {
			return nil, fmt.Errorf("failed to reg metrics, %w", err)
		}
		return h, nil
	})
}

type Limit struct {
	ctx  router.PluginCtx
	next router.Middleware
	args Args

	concurrent atomic.Int32
	rate       *rate.Limiter // Nil if no qps limit

	rejectedCcTotal  prometheus.Counter
	rejectedQpsTotal prometheus.Counter
}

func (h *Limit) Handle(ctx context.Context, q *router.QueryCtx) {
	if h.rate != nil {
		if !h.rate.Allow() {
			h.rejectedQpsTotal.Inc()
			router.SetEmptyRespMQ(q, dnsmsg.RCodeRefused)
			return
		}
	}

	if ccLimit := h.args.Concurrent; ccLimit > 0 {
		cc := h.concurrent.Add(1)
		// Register the decrement immediately after the increment. It used to sit
		// after the over-limit check, so a rejected query returned without ever
		// decrementing -- the counter kept the leaked increment forever. Once
		// enough bursts leaked past the limit, the counter stayed above ccLimit
		// permanently and every subsequent query was REFUSED until restart,
		// turning a transient spike into a permanent self-inflicted outage.
		defer h.concurrent.Add(-1)
		if cc > ccLimit {
			h.rejectedCcTotal.Inc()
			router.SetEmptyRespMQ(q, dnsmsg.RCodeRefused)
			return
		}
	}
	h.next.Handle(ctx, q)
}
