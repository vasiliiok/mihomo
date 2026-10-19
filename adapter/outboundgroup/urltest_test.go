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

// fakeDelayProxy is a proxy with a fixed health-check result.
type fakeDelayProxy struct {
	C.Proxy
	name  string
	alive bool
	delay uint16
}

func (p *fakeDelayProxy) Name() string                      { return p.name }
func (p *fakeDelayProxy) AliveForTestUrl(string) bool       { return p.alive }
func (p *fakeDelayProxy) LastDelayForTestUrl(string) uint16 { return p.delay }

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

func delayProxies(delays ...uint16) []C.Proxy {
	proxies := make([]C.Proxy, 0, len(delays))
	for i, delay := range delays {
		proxies = append(proxies, &fakeDelayProxy{
			Proxy: adapter.NewProxy(outbound.NewDirect()),
			name:  fmt.Sprintf("node-%d", i+1),
			alive: true,
			delay: delay,
		})
	}
	return proxies
}

func TestURLTestFollowsRecreatedProviderProxies(t *testing.T) {
	pd := &fakeProxyProvider{}
	pd.update(delayProxies(100, 50, 120))

	group, err := NewURLTest(
		GroupCommonOption{Name: "url-test", URL: testUrl},
		URLTestOption{Tolerance: 30},
		adapter.NewProxy(outbound.NewDirect()),
		[]P.ProxyProvider{pd},
	)
	require.NoError(t, err)
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
