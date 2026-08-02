package router

import (
	"encoding/binary"

	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
)

func SetEmptyRespMQ(q *QueryCtx, rcode dnsmsg.RCode) {
	resp := dnsmsg.NewMsg()
	resp.RCode = rcode
	resp.Questions = append(resp.Questions, q.Question.Copy())
	q.SetResp(resp)
}

// append cache key for this query to b.
func (r *Router) appendCacheKey(b []byte, q *QueryCtx, routeTag string) []byte {
	qName := q.Question.Name.Data()
	b = append(b, byte(len(qName)))
	b = append(b, qName...)
	b = binary.BigEndian.AppendUint16(b, uint16(q.Question.Class))
	b = binary.BigEndian.AppendUint16(b, uint16(q.Question.Type))
	// 同一个域名在规则更新前后可能从动态/国外切到国内，或反向切换。
	// 不把实际规则目标写进 key，reload 后仍会命中旧路径留下的乐观缓存，
	// 最长可持续 optimistic_ttl。路由标签很短，换取规则立即生效是值得的。
	b = append(b, byte(len(routeTag)))
	b = append(b, routeTag...)

	p := len(b)
	b = append(b, 0)
	switch {
	case len(q.ECSZone) > 0:
		b = append(b, 1)
		b = append(b, q.ECSZone...)
	case q.ECS2Upstream.IsValid():
		b = append(b, 2)
		// AppendTo follows Go's append convention: it may reallocate and returns
		// the resulting slice. Dropping that return value silently discarded the
		// ECS prefix, so it never became part of the cache key -- every client
		// shared a single cache entry regardless of its subnet, and the length
		// byte written below covered only the type marker.
		b = q.ECS2Upstream.Masked().AppendTo(b)
	}
	b[p] = byte(len(b) - p)
	return b
}
