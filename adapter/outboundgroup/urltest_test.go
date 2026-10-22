package outboundgroup

import (
	"fmt"
	"testing"

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
