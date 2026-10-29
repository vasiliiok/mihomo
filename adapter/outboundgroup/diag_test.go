package outboundgroup

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/diag"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"

	"github.com/stretchr/testify/require"
)

// diagLines turns the diagnostic log on for the test and returns a reader of its lines.
func diagLines(t *testing.T) func() []string {
	t.Helper()
	d := t.TempDir()
	diag.SetDir(d)
	t.Cleanup(func() { diag.SetDir("") })
	return func() []string {
		files, _ := filepath.Glob(filepath.Join(d, "diag-*.log"))
		var lines []string
		for _, f := range files {
			b, err := os.ReadFile(f)
			require.NoError(t, err)
			lines = append(lines, strings.Split(strings.TrimSpace(string(b)), "\n")...)
		}
		return lines
	}
}

func grep(lines []string, parts ...string) []string {
	var out []string
	for _, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}

func TestURLTestDiagLogsDecisions(t *testing.T) {
	lines := diagLines(t)
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(100, 50, 120))
	group := newTestURLTest(t, pd, 80)
	require.Equal(t, "node-2", group.fast(false).Name())

	// recreated, node-2 not tested yet: kept on its previous result
	pd.update([]C.Proxy{newDelayProxy("node-1", true, 40), newDelayProxy("node-2", false, 0), newDelayProxy("node-3", true, 120)})
	group.fastSingle.Reset()
	require.Equal(t, "node-2", group.fast(false).Name())

	// node-2 tested and slow: leave it
	pd.update(delayProxies(40, 300, 120))
	group.fastSingle.Reset()
	require.Equal(t, "node-1", group.fast(false).Name())

	// the same decision again is not logged
	group.fastSingle.Reset()
	group.fast(false)

	got := grep(lines(), " url-test ")
	require.Len(t, got, 3, strings.Join(got, "\n"))
	require.Contains(t, got[0], `from="" to="node-2" reason=first`)
	require.Contains(t, got[1], `from="node-2" to="node-2" reason=pending from_delay=50`)
	require.Contains(t, got[2], `from="node-2" to="node-1" reason=slower from_delay=300 to_delay=40 best="node-1" best_delay=40 tol=80`)
}

func TestURLTestDiagStatusReportsDelta(t *testing.T) {
	lines := diagLines(t)
	pd := &fakeProxyProvider{}
	proxies := delayProxies(100, 50)
	pd.update(proxies)
	group := newTestURLTest(t, pd, 30)
	require.Equal(t, "node-2", group.fast(false).Name())

	// node-1 gets faster than node-2 by more than the tolerance, but the
	// cached decision is still node-2
	proxies[0].(*fakeDelayProxy).delay = 10
	status := group.DiagStatus()
	require.Contains(t, status, `url-test="url-test" now="node-2" delay=50 best="node-1" best_delay=10 delta=40 tol=30 alive=2 dead=0 untested=0 total=2`)
	require.Len(t, grep(lines(), " warn ", `"node-2" is 40 ms slower than "node-1"`), 1)
}

func TestFallbackDiagLogsSwitches(t *testing.T) {
	lines := diagLines(t)
	a := newDelayProxy("a", true, 100)
	b := newDelayProxy("b", true, 200)
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{a, b})
	fb, err := NewFallback(GroupCommonOption{Name: "fb", URL: testUrl}, FallbackOption{},
		adapter.NewProxy(outbound.NewDirect()), []P.ProxyProvider{pd})
	require.NoError(t, err)

	require.Equal(t, "a", fb.findAliveProxy(false).Name())
	require.Equal(t, "a", fb.findAliveProxy(false).Name())
	a.alive = false
	require.Equal(t, "b", fb.findAliveProxy(false).Name())

	got := grep(lines(), " fallback ")
	require.Len(t, got, 2, strings.Join(got, "\n"))
	require.Contains(t, got[1], `group="fb" from="a" to="b" fixed="" members=["a":dead "b":200]`)
	require.Contains(t, fb.DiagStatus(), `fallback="fb" now="b" chain="b" leaf_delay=200`)
}

func TestURLTestDiagMarksLoadedAndStale(t *testing.T) {
	lines := diagLines(t)
	pd := &fakeProxyProvider{}
	pd.update(nil) // provider not loaded yet: the group hands out its empty fallback
	group := newTestURLTest(t, pd, 30)
	require.Same(t, group.EmptyFallback(), group.fast(false))
	require.Contains(t, group.DiagStatus(), "empty_fallback=true")
	require.Empty(t, grep(lines(), " warn "), "no proxies at all: nothing to warn about")

	// the proxies arrive while the empty fallback is still cached
	pd.update(delayProxies(100, 50))
	require.Contains(t, group.DiagStatus(), "empty_fallback=true")
	require.Len(t, grep(lines(), " warn ", "hands out its empty fallback", "while 2 of its proxies are alive"), 1)

	pd.update(delayProxies(100, 50))
	group.fastSingle.Reset()
	require.Equal(t, "node-2", group.fast(false).Name())
	require.Len(t, grep(lines(), " url-test ", `to="node-2" reason=loaded`), 1)

	// the provider replaces its proxies while the pick is still cached
	pd.update(delayProxies(100, 10))
	status := group.DiagStatus()
	require.Contains(t, status, "stale_object=true")
	require.Empty(t, grep(lines(), " warn ", "slower than"), "a cached replaced object must not raise a delta warning")
}

func TestURLTestDiagLogsReliability(t *testing.T) {
	lines := diagLines(t)
	near := historyProxy("near", 100, 100, 100, 100, 100, 100, 100, 100, 0, 100)
	sound := historyProxy("sound", 120, 120, 120, 120, 120, 120, 120, 120, 120, 120)
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{near, sound})
	group := newRatedURLTest(t, pd, URLTestOption{Tolerance: 80, Reliability: 90, DelaySamples: 3})
	require.Same(t, near, group.fast(false))

	near.history = historyProxy("", 100, 100, 100, 100, 100, 100, 0, 100, 0, 300).history
	group.fastSingle.Reset()
	require.Same(t, sound, group.fast(false))
	require.Contains(t, group.DiagConfig(), "tolerance=80 reliability=90 delay_samples=3 ")
	status := group.DiagStatus()
	require.Contains(t, status, `now="sound" delay=120 best="sound" best_delay=120 delta=0`)
	require.Contains(t, status, "ok=10/10 last=120 best_ok=10/10 unreliable=1")

	got := grep(lines(), " url-test ")
	require.Len(t, got, 2, strings.Join(got, "\n"))
	require.Contains(t, got[0], `to="near" reason=first`)
	require.Contains(t, got[1], `from="near" to="sound" reason=unreliable from_delay=100 to_delay=120 best="sound" best_delay=120 tol=80 from_ok=8/10 to_ok=10/10 best_ok=10/10 from_last=300`)
}

func TestFallbackDiagLogsConfirmation(t *testing.T) {
	lines := diagLines(t)
	main, spare := historyProxy("main", 80), historyProxy("spare", 90)
	main.results = []uint16{0}
	group := newTestFallback(t, true, main, spare)
	require.Contains(t, group.DiagConfig(), " fail_confirm=true ")

	main.check(0)
	require.Contains(t, group.DiagStatus(), `members=["main":suspect "spare":90]`)
	require.Eventually(t, func() bool { return len(grep(lines(), " fallback-confirm ")) == 1 }, 3*time.Second, 10*time.Millisecond)
	require.Contains(t, group.DiagStatus(), `now="spare"`)
	got := grep(lines(), " fallback-confirm ")
	require.Contains(t, got[0], `group="fallback" member="main" result=dead after=`)
	require.Contains(t, got[0], `err="timeout"`)
	require.Contains(t, grep(lines(), " fallback ")[1], `from="main" to="spare" fixed="" members=["main":dead "spare":90]`)
}

// earlyConn is a connection whose handshake waits for the first write, like vless.
type earlyConn struct{ C.Conn }

func (earlyConn) NeedHandshake() bool { return true }

type earlyProxy struct {
	*fakeDelayProxy
	writeErr error
}

func (p *earlyProxy) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	if p.writeErr != nil {
		_ = a.Close()
	}
	return earlyConn{outbound.NewConn(a, p.fakeDelayProxy)}, nil
}

func TestGroupDiagCountsDialsAfterHandshakeAndApartFromHealthChecks(t *testing.T) {
	_ = diagLines(t)
	node := &earlyProxy{fakeDelayProxy: newDelayProxy("node-1", true, 50)}
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{node})
	group := newTestURLTest(t, pd, 80)
	metadata := &C.Metadata{}

	// dialed but nothing written yet: not counted
	c, err := group.DialContext(context.Background(), metadata)
	require.NoError(t, err)
	require.Contains(t, group.diagDials(), "dial_ok=0 dial_fail=0 hc_ok=0 hc_fail=0")
	_, err = c.Write([]byte("x"))
	require.NoError(t, err)
	_ = c.Close()

	// the handshake fails on the first write
	node.writeErr = io.ErrClosedPipe
	c, err = group.DialContext(context.Background(), metadata)
	require.NoError(t, err)
	_, _ = c.Write([]byte("x"))

	// a health check of a group that uses this one
	node.writeErr = nil
	c, err = group.DialContext(diag.MarkHealthCheck(context.Background()), metadata)
	require.NoError(t, err)
	_, _ = c.Write([]byte("x"))
	_ = c.Close()

	require.Contains(t, group.diagDials(), "dial_ok=1 dial_fail=1 hc_ok=1 hc_fail=0")
}
