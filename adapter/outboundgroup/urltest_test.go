package outboundgroup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"

	"github.com/stretchr/testify/require"
)

// fakeDelayProxy is a proxy with a fixed health-check result. Like a freshly
// created adapter.Proxy, an untested one is alive and has the maximum delay.
type fakeDelayProxy struct {
	C.Proxy
	name   string
	tested bool
	alive  bool
	delay  uint16
}

func (p *fakeDelayProxy) Name() string { return p.name }

func (p *fakeDelayProxy) AliveForTestUrl(string) bool { return !p.tested || p.alive }

func (p *fakeDelayProxy) LastDelayForTestUrl(string) uint16 {
	if !p.tested || !p.alive {
		return 0xffff
	}
	return p.delay
}

func (p *fakeDelayProxy) ExtraDelayHistories() map[string]C.ProxyState {
	if !p.tested {
		return map[string]C.ProxyState{}
	}
	return map[string]C.ProxyState{
		testUrl: {Alive: p.alive, History: []C.DelayHistory{{Delay: p.delay}}},
	}
}

// fakeProxyProvider imitates a proxy-provider whose subscription content changes:
// every update brings brand-new proxy objects with the same names.
type fakeProxyProvider struct {
	proxies []C.Proxy
	version uint32
}

func (pp *fakeProxyProvider) Name() string               { return "fake" }
func (pp *fakeProxyProvider) VehicleType() P.VehicleType { return P.File }
func (pp *fakeProxyProvider) Type() P.ProviderType       { return P.Proxy }
func (pp *fakeProxyProvider) Initial() error             { return nil }
func (pp *fakeProxyProvider) Update() error              { return nil }
func (pp *fakeProxyProvider) Proxies() []C.Proxy         { return pp.proxies }
func (pp *fakeProxyProvider) Count() int                 { return len(pp.proxies) }
func (pp *fakeProxyProvider) Touch()                     {}
func (pp *fakeProxyProvider) HealthCheck()               {}
func (pp *fakeProxyProvider) Version() uint32            { return pp.version }
func (pp *fakeProxyProvider) HealthCheckURL() string     { return testUrl }
func (pp *fakeProxyProvider) RegisterHealthCheckTask(string, utils.IntRanges[uint16], string, uint) {
}

func (pp *fakeProxyProvider) update(proxies []C.Proxy) {
	pp.proxies = proxies
	pp.version++
}

func newDelayProxy(name string, tested bool, delay uint16) *fakeDelayProxy {
	return &fakeDelayProxy{
		Proxy:  adapter.NewProxy(outbound.NewDirect()),
		name:   name,
		tested: tested,
		alive:  true,
		delay:  delay,
	}
}

// delayProxies returns tested proxies node-1, node-2, … with the given delays.
func delayProxies(delays ...uint16) []C.Proxy {
	proxies := make([]C.Proxy, 0, len(delays))
	for i, delay := range delays {
		proxies = append(proxies, newDelayProxy(fmt.Sprintf("node-%d", i+1), true, delay))
	}
	return proxies
}

func newTestURLTest(t *testing.T, pd P.ProxyProvider, tolerance uint16) *URLTest {
	t.Helper()
	group, err := NewURLTest(
		GroupCommonOption{Name: "url-test", URL: testUrl},
		URLTestOption{Tolerance: tolerance},
		adapter.NewProxy(outbound.NewDirect()),
		[]P.ProxyProvider{pd},
	)
	require.NoError(t, err)
	return group
}

func TestURLTestFollowsRecreatedProviderProxies(t *testing.T) {
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(100, 50, 120))
	group := newTestURLTest(t, pd, 30)
	require.Equal(t, "node-2", group.fast(false).Name())

	// Subscription update with the same delays: the group must hand out
	// the current object, not the one from the previous generation.
	second := delayProxies(100, 50, 120)
	pd.update(second)
	group.fastSingle.Reset()
	require.Same(t, second[1], group.fast(false),
		"url-test returned a proxy object the provider has already replaced")

	// Subscription update where node-2 became slow: the group must judge
	// node-2 by its current object and move to node-1.
	third := delayProxies(100, 300, 120)
	pd.update(third)
	group.fastSingle.Reset()
	require.Same(t, third[0], group.fast(false),
		"url-test kept a node using the frozen delay of a replaced object")
}

func TestURLTestKeepsRecreatedProxyUntilItIsTested(t *testing.T) {
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(100, 50, 120))
	group := newTestURLTest(t, pd, 80)
	require.Equal(t, "node-2", group.fast(false).Name())

	// Right after a subscription update nothing is tested yet: every delay reads
	// as 0xffff. The group must stay on node-2 (0xffff + tolerance used to wrap
	// around in uint16 and move it to the first proxy).
	untested := []C.Proxy{
		newDelayProxy("node-1", false, 0),
		newDelayProxy("node-2", false, 0),
		newDelayProxy("node-3", false, 0),
	}
	pd.update(untested)
	group.fastSingle.Reset()
	require.Same(t, untested[1], group.fast(false),
		"url-test left its node before any proxy was tested")

	// The health check has reached node-1 and node-3 but not node-2 yet.
	partly := []C.Proxy{
		newDelayProxy("node-1", true, 40),
		newDelayProxy("node-2", false, 0),
		newDelayProxy("node-3", true, 120),
	}
	pd.update(partly)
	group.fastSingle.Reset()
	require.Same(t, partly[1], group.fast(false),
		"url-test left its node only because that node was not tested yet")

	// Once node-2 is tested, tolerance decides as usual.
	tested := delayProxies(40, 300, 120)
	pd.update(tested)
	group.fastSingle.Reset()
	require.Same(t, tested[0], group.fast(false),
		"url-test did not move away from a tested slow node")
}

func TestURLTestToleranceAppliesToFirstProxy(t *testing.T) {
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(50, 200))
	group := newTestURLTest(t, pd, 80)
	require.Equal(t, "node-1", group.fast(false).Name())

	// node-2 is faster, but by less than the tolerance: stay on node-1.
	next := delayProxies(100, 60)
	pd.update(next)
	group.fastSingle.Reset()
	require.Same(t, next[0], group.fast(false),
		"tolerance was ignored because the selected node is the first one")
}

func TestURLTestLeavesProxyThatIsNeverTested(t *testing.T) {
	node1 := newDelayProxy("node-1", false, 0)
	node2 := newDelayProxy("node-2", false, 0)
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{node1, node2})
	group := newTestURLTest(t, pd, 80)

	// Nothing is tested yet, so the group starts on the first proxy.
	require.Same(t, node1, group.fast(false))

	// node-2 gets tested, node-1 never does (for example, the provider tests
	// testUrl only for proxies matching another group's filter). Such a node has
	// no earlier result to go by and must not be kept forever.
	node2.tested, node2.delay = true, 50
	group.fastSingle.Reset()
	require.Same(t, node2, group.fast(false),
		"url-test stayed on a proxy that has never been tested")
}

func TestURLTestLeavesRecreatedProxyThatWasDead(t *testing.T) {
	pd := &fakeProxyProvider{}
	first := delayProxies(100, 50)
	pd.update(first)
	group := newTestURLTest(t, pd, 80)
	require.Equal(t, "node-2", group.fast(false).Name())

	// node-2 fails its health check, and the subscription is updated before the
	// group picks again. The new node-2 is not tested yet, but its last result
	// says it is dead.
	first[1].(*fakeDelayProxy).alive = false
	next := []C.Proxy{
		newDelayProxy("node-1", true, 100),
		newDelayProxy("node-2", false, 0),
	}
	pd.update(next)
	group.fastSingle.Reset()
	require.Same(t, next[0], group.fast(false),
		"url-test kept a recreated proxy whose last result was a failure")
}

// fakeHistoryProxy is a proxy with a health check history for testUrl; a zero
// delay is a failed check. Like adapter.Proxy, the last check decides alive.
// URLTest records the next of the scripted results.
type fakeHistoryProxy struct {
	C.Proxy
	name    string
	mu      sync.Mutex
	history []C.DelayHistory
	results []uint16 // what URLTest will measure, 0 = fail
	tests   int
}

func historyProxy(name string, delays ...uint16) *fakeHistoryProxy {
	p := &fakeHistoryProxy{Proxy: adapter.NewProxy(outbound.NewDirect()), name: name}
	for _, d := range delays {
		p.history = append(p.history, C.DelayHistory{Delay: d})
	}
	return p
}

func (p *fakeHistoryProxy) Name() string { return p.name }

func (p *fakeHistoryProxy) AliveForTestUrl(string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.history) == 0 || p.history[len(p.history)-1].Delay > 0
}

func (p *fakeHistoryProxy) LastDelayForTestUrl(string) uint16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.history) == 0 || p.history[len(p.history)-1].Delay == 0 {
		return 0xffff
	}
	return p.history[len(p.history)-1].Delay
}

func (p *fakeHistoryProxy) ExtraDelayHistories() map[string]C.ProxyState {
	alive := p.AliveForTestUrl(testUrl)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.history) == 0 {
		return map[string]C.ProxyState{}
	}
	return map[string]C.ProxyState{testUrl: {Alive: alive, History: append([]C.DelayHistory(nil), p.history...)}}
}

// check records a health check result, as a provider round would.
func (p *fakeHistoryProxy) check(delay uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = append(p.history, C.DelayHistory{Time: time.Now(), Delay: delay})
}

func (p *fakeHistoryProxy) URLTest(context.Context, string, utils.IntRanges[uint16]) (uint16, error) {
	p.mu.Lock()
	var delay uint16
	if len(p.results) > 0 {
		delay, p.results = p.results[0], p.results[1:]
	}
	p.tests++
	p.mu.Unlock()
	p.check(delay)
	if delay == 0 {
		return 0, errors.New("timeout")
	}
	return delay, nil
}

func (p *fakeHistoryProxy) testCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tests
}

func newRatedURLTest(t *testing.T, pd P.ProxyProvider, opt URLTestOption) *URLTest {
	t.Helper()
	group, err := NewURLTest(GroupCommonOption{Name: "url-test", URL: testUrl}, opt, adapter.NewProxy(outbound.NewDirect()), []P.ProxyProvider{pd})
	require.NoError(t, err)
	return group
}

func TestURLTestDelaySamplesIgnoreSingleSpike(t *testing.T) {
	fast := historyProxy("fast", 60, 65, 60)
	steady := historyProxy("steady", 150, 150, 150)
	for _, samples := range []int{0, 3} {
		pd := &fakeProxyProvider{}
		pd.update([]C.Proxy{steady, fast})
		group := newRatedURLTest(t, pd, URLTestOption{Tolerance: 80, DelaySamples: samples})
		require.Same(t, fast, group.fast(false))

		// one check of the selected node is slow
		fast.history = append(fast.history, C.DelayHistory{Delay: 310})
		group.fastSingle.Reset()
		if samples == 0 {
			require.Same(t, steady, group.fast(false), "without delay-samples the last delay decides")
		} else {
			require.Same(t, fast, group.fast(false), "a single slow check moved the group off its node")
		}
		fast.history = fast.history[:3]
	}

	// a node that stays slow is left: the median follows it after two checks
	fast.history = append(fast.history, C.DelayHistory{Delay: 310}, C.DelayHistory{Delay: 320})
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{steady, fast})
	group := newRatedURLTest(t, pd, URLTestOption{Tolerance: 80, DelaySamples: 3})
	group.fastNode = fast
	require.Same(t, steady, group.fast(false), "the median did not follow a node that became slow")
}

func TestURLTestPrefersReliableProxies(t *testing.T) {
	flaky := historyProxy("flaky", 50, 0, 50, 50, 0, 50, 50, 50, 0, 50) // 7 of 10
	sound := historyProxy("sound", 120, 120, 120, 120, 120, 120, 120, 120, 120, 120)
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{flaky, sound})

	group := newRatedURLTest(t, pd, URLTestOption{Tolerance: 80})
	require.Same(t, flaky, group.fast(false), "without reliability the lowest delay wins")

	group = newRatedURLTest(t, pd, URLTestOption{Tolerance: 80, Reliability: 90})
	require.Same(t, sound, group.fast(false), "an unreliable node was preferred to a reliable one")

	// the selected node fails a second check of ten: it becomes unreliable and is left
	// even though its delay is within the tolerance
	near := historyProxy("near", 100, 0, 100, 100, 100, 100, 100, 100, 100, 100)
	pd.update([]C.Proxy{near, sound})
	group = newRatedURLTest(t, pd, URLTestOption{Tolerance: 80, Reliability: 90})
	require.Same(t, near, group.fast(false), "one failure in ten is within reliability 90")
	near.history = historyProxy("", 100, 0, 100, 100, 100, 100, 100, 0, 100, 100).history
	group.fastSingle.Reset()
	require.Same(t, sound, group.fast(false), "the group kept a node that became unreliable")
}

func TestURLTestWithoutReliableProxiesPicksTheFastest(t *testing.T) {
	a := historyProxy("a", 50, 0, 50, 0, 50)
	b := historyProxy("b", 0, 0, 90, 90, 90)
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{b, a})
	group := newRatedURLTest(t, pd, URLTestOption{Tolerance: 10, Reliability: 90})
	require.Same(t, a, group.fast(false))
}

func TestURLTestReliabilityDoesNotPreferUntested(t *testing.T) {
	untested := historyProxy("untested")
	tested := historyProxy("tested", 0, 200, 200) // 2 of 3: unreliable
	pd := &fakeProxyProvider{}
	pd.update([]C.Proxy{untested, tested})
	group := newRatedURLTest(t, pd, URLTestOption{Reliability: 90})
	require.Same(t, tested, group.fast(false), "an untested node was preferred to a tested alive one")
}

func TestURLTestClampsRatingOptions(t *testing.T) {
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(10))
	group := newRatedURLTest(t, pd, URLTestOption{Reliability: 150, DelaySamples: 50})
	require.Equal(t, 100, group.reliability)
	require.Equal(t, 10, group.delaySamples)
	group = newRatedURLTest(t, pd, URLTestOption{Reliability: -5, DelaySamples: -1})
	require.Equal(t, 0, group.reliability)
	require.Equal(t, 0, group.delaySamples)
}
