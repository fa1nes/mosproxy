package router

import (
	"bytes"
	"testing"

	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
)

func TestAppendCacheKeyIncludesRouteTag(t *testing.T) {
	q := &QueryCtx{}
	if err := q.Question.Name.Parse("example.com."); err != nil {
		t.Fatal(err)
	}
	q.Question.Class = dnsmsg.ClassINET
	q.Question.Type = dnsmsg.TypeA

	r := &Router{}
	cnKey := r.appendCacheKey(nil, q, "cn-lb")
	foreignKey := r.appendCacheKey(nil, q, "foreign-hk")
	cnKeyAgain := r.appendCacheKey(nil, q, "cn-lb")

	if bytes.Equal(cnKey, foreignKey) {
		t.Fatal("different forward routes must not share a cache key")
	}
	if !bytes.Equal(cnKey, cnKeyAgain) {
		t.Fatal("the same query and forward route must produce a stable cache key")
	}
}
