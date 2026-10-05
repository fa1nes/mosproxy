package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IrineSistiana/mosproxy/internal/cache"
	"github.com/IrineSistiana/mosproxy/internal/mlog"
	"github.com/IrineSistiana/mosproxy/pkg/dnsmsg"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func wireName(t *testing.T, s string) []byte {
	t.Helper()
	n := dnsmsg.NewName()
	require.NoError(t, n.Parse(s))
	return append([]byte(nil), n.Data()...)
}

func cacheKeyFor(t *testing.T, s string) []byte {
	name := wireName(t, s)
	return append(append([]byte{byte(len(name))}, name...), 0, 1, 0, 1, 3, 'l', 'b', '0')
}

func TestKeyUnderNameStopsAtLabelBoundaries(t *testing.T) {
	target := wireName(t, "sb.sb")
	for name, want := range map[string]bool{
		"sb.sb": true, "www.sb.sb": true, "a.b.sb.sb": true,
		"xsb.sb": false, "sb.sbx": false, "sb.sb.cn": false, "example.com": false,
		`x\002sb.sb`: false,
	} {
		require.Equal(t, want, keyUnderName(cacheKeyFor(t, name), target), name)
	}
	require.False(t, keyUnderName(nil, target))
	require.False(t, keyUnderName([]byte{40, 1, 2}, target))
}

func TestFlushRemovesOnlyTheDomainAndItsSubdomains(t *testing.T) {
	memory, err := cache.NewMemoryCache(1 << 20)
	require.NoError(t, err)
	defer memory.Close()
	ctl := &CacheCtl{memory: memory}
	now := time.Now().Unix()
	times := cache.Times{StoredAtUnix: now, ExpireAtUnix: now + 300, CacheExpireAtUnix: now + 300}
	for _, name := range []string{"sb.sb", "www.sb.sb", "example.com", "xsb.sb"} {
		memory.Store(cacheKeyFor(t, name), []byte("v"), times, false)
	}
	require.Eventually(t, func() bool {
		v, _ := memory.Get(cacheKeyFor(t, "xsb.sb"))
		return v != nil
	}, time.Second, 10*time.Millisecond)

	require.Equal(t, 2, ctl.Flush(wireName(t, "sb.sb.")))
	for name, kept := range map[string]bool{"sb.sb": false, "www.sb.sb": false, "example.com": true, "xsb.sb": true} {
		v, _ := memory.Get(cacheKeyFor(t, name))
		require.Equal(t, kept, v != nil, name)
	}
	require.Equal(t, 2, ctl.Flush(nil))
	require.Equal(t, 0, (&CacheCtl{}).Flush(nil))
}

func TestFlushWithoutACacheAnswersInsteadOfPanicking(t *testing.T) {
	r := &Router{logger: mlog.L(), metricsReg: prometheus.NewRegistry(), apiMux: chi.NewMux()}
	require.NoError(t, r.initApiServer(&APIConfig{Addr: "127.0.0.1:0"}))
	defer func() {
		for _, closer := range r.serverClosers {
			closer()
		}
	}()
	w := httptest.NewRecorder()
	r.apiMux.ServeHTTP(w, httptest.NewRequest("GET", "/ctl/flush?domain=sb.sb", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}
