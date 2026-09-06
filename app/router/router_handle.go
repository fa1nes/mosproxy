package router

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/IrineSistiana/mosproxy/internal/pool"
	"github.com/IrineSistiana/mosproxy/internal/utils"
	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
)

// always set q.Resp
func (r *Router) serverEntryHandler(q *QueryCtx) {
	// Set info about ECS
	// Priority: client addr < forward client ecs < overwrite
	if r.opt.ECS.Enabled {
		if q.RemoteAddr.IsValid() {
			addr := q.RemoteAddr.Addr().Unmap()
			if addr.Is4() {
				q.ECS2Upstream = netip.PrefixFrom(addr, 24)
			} else {
				q.ECS2Upstream = netip.PrefixFrom(addr, 48)
			}
		}
		if r.opt.ECS.Forward && q.ClientECS.IsValid() { //
			q.ECS2Upstream = q.ClientECS
		}

		// Get zone
		if r.ecsZone != nil {
			q.ECSZone, _ = r.ecsZone.Mark(q.ECS2Upstream.Addr())

			// An address the zone file does not cover is not evidence that the
			// client is local -- only a non-global address is. Clearing ECS for
			// every unmarked address silently disables subnet-aware answers for
			// all public space the zone file happens to miss, and appendCacheKey
			// already shards by the raw prefix when the zone name is empty.
			if len(q.ECSZone) == 0 && !ecsAddrIsGlobal(q.ECS2Upstream.Addr()) {
				q.ECS2Upstream = netip.Prefix{}
			}
		}

		// Overwrite ecs
		if r.ecsZoneOverwrite != nil {
			p, ok := r.ecsZoneOverwrite.Get(q.ECSZone)
			if ok {
				q.ECS2Upstream = p
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.ctx, queryTimeout)
	defer cancel()

	if len(r.middlewares) > 0 {
		r.middlewares[0].Handle(ctx, q)
	} else {
		r.BuiltInHandler(ctx, q)
	}

	if q.Resp() == nil {
		SetEmptyRespMQ(q, dnsmsg.RCodeRefused)
	}
	if r.opt.Log.Queries {
		r.logAccess(q)
	}
}

var cacheKeyPool = pool.NewBytesPool()

// router main handle func.
func (r *Router) BuiltInHandler(ctx context.Context, q *QueryCtx) {
	// Match rules
	var matchedRule *rule
	for _, rule := range r.rules {
		if rule.match(q) {
			matchedRule = rule
			break
		}
	}

	if matchedRule == nil {
		SetEmptyRespMQ(q, dnsmsg.RCodeRefused)
		return
	}
	if rejectRCode := matchedRule.cfg.Reject; rejectRCode > 0 {
		SetEmptyRespMQ(q, dnsmsg.RCode(rejectRCode))
		return
	}
	upstream := matchedRule.upstream
	if upstream == nil {
		SetEmptyRespMQ(q, dnsmsg.RCodeRefused)
		return
	}

	// lookup cache
	ckb := cacheKeyPool.Get()
	defer cacheKeyPool.Release(ckb)
	ckb.B = r.appendCacheKey(ckb.B, q, matchedRule.cacheTag)
	resp, t := r.cache.Get(ctx, ckb.B)
	if resp != nil {
		if r.needPrefetch(t) {
			r.AsyncSingleFlightPrefetch(ckb.B, q, upstream)
		}
		r.queryCacheHitTotal.Inc()
		q.SetRespFrom(resp, "cache")
		return
	}

	if ctxDone(ctx) { // check if redis server timed out
		SetEmptyRespMQ(q, dnsmsg.RCodeServerFailure)
		return
	}

	err := r.forward(ctx, q, upstream)
	if err != nil {
		SetEmptyRespMQ(q, dnsmsg.RCodeServerFailure)
		return
	}
	r.cache.Store(ckb.B, q.Resp())
}

// Prefetching q in other goroutine.
// If a query with same key is currently prefetching, do nothing.
func (r *Router) AsyncSingleFlightPrefetch(key []byte, q *QueryCtx, u Upstream) {
	if len(key) == 0 {
		return
	}
	sk, ok := r.prefetchSf.Reserve(key)
	if !ok {
		return
	}
	qCopy := q.Copy()
	go func() {
		defer ReleaseQueryCtx(qCopy)
		// Release the single-flight reservation when this prefetch finishes.
		// Without it, Reserve() succeeds exactly once per key for the whole
		// lifetime of the process: the entry is never removed from the queue, so
		// every later attempt returns false and that key can never be refreshed
		// again. With optimistic caching enabled this means a stale record keeps
		// being served for the entire optimistic window. The map also grows
		// without bound, which is remotely reachable via random subdomains.
		defer r.prefetchSf.Done(sk)
		r.DoPrefetch(utils.Str2BytesUnsafe(sk), qCopy, u)
	}()
}

// Send q to u, and save response under key.
func (r *Router) DoPrefetch(key []byte, q *QueryCtx, u Upstream) {
	if len(key) == 0 {
		return
	}

	q.Prefetch = true
	ctx, cancel := context.WithTimeout(r.ctx, prefetchTimeout)
	defer cancel()
	err := r.forward(ctx, q, u)
	if err != nil {
		return
	}
	r.prefetchTotal.Inc()
	r.cache.Store(key, q.Resp())
}

// forward query to upstream and set the response.
// Will remove edns0 from resp, and the ech SvcParam unless disabled.
func (r *Router) forward(ctx context.Context, q *QueryCtx, upstream Upstream) error {
	m := r.MakeQueryMsg(q)
	defer dnsmsg.ReleaseMsg(m)

	err := upstream.Exchange(ctx, q, m)
	if err != nil {
		return fmt.Errorf("failed to exchange, %w", err)
	}
	if resp := q.Resp(); resp != nil {
		dnsmsg.RemoveEDNS0(resp)
		// Strip here, before the caller stores the response: this is the single point
		// both the live path and DoPrefetch go through, and rewriting once on the way in
		// beats rewriting on every cache hit on the way out.
		if r.stripECHEnabled() && dnsmsg.StripECH(resp) {
			r.echStrippedTotal.Inc()
		}
	}
	return nil
}

// stripECHEnabled reports whether ech removal is on. Absent config means on — see the
// comment on Config.StripECH for why this is not a plain bool.
func (r *Router) stripECHEnabled() bool {
	return r.opt.StripECH == nil || *r.opt.StripECH
}

// Make a dns msg from q, according to r's settings.
func (r *Router) MakeQueryMsg(q *QueryCtx) *dnsmsg.Msg {
	m := dnsmsg.NewMsg()
	m.Header.RecursionDesired = true
	m.Questions = append(m.Questions, q.Question.Copy())

	opt := newEDNS0(udpSize)
	if ecs := q.ECS2Upstream; ecs.IsValid() {
		addr := ecs.Addr()
		if !addr.IsPrivate() && addr.IsGlobalUnicast() {
			opt.Data = makeEdns0ClientSubnetReqOpt(ecs)
		}
	}
	m.Additionals = append(m.Additionals, opt)
	return m
}
