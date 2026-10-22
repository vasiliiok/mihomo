package provider

import (
	"context"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"

	"github.com/stretchr/testify/require"
)

// reportingProxy reports every health check of it and can hold the check
// until release is closed.
type reportingProxy struct {
	C.Proxy
	name    string
	tested  chan<- string
	release <-chan struct{}
}

func (p *reportingProxy) Name() string { return p.name }

func (p *reportingProxy) URLTest(ctx context.Context, url string, expectedStatus utils.IntRanges[uint16]) (uint16, error) {
	p.tested <- p.name
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
	return 1, nil
}

func TestHealthCheckTestsProxiesReplacedDuringACheck(t *testing.T) {
	tested := make(chan string, 16)
	release := make(chan struct{})
	defer close(release)

	old := []C.Proxy{&reportingProxy{Proxy: adapter.NewProxy(outbound.NewDirect()), name: "old", tested: tested, release: release}}
	hc := NewHealthCheck(old, "https://www.gstatic.com/generate_204", 5000, 300, false, nil)
	defer hc.close()
	bp := &baseProvider{name: "test", proxies: old, healthCheck: hc}

	go hc.check()
	select {
	case name := <-tested:
		require.Equal(t, "old", name)
	case <-time.After(3 * time.Second):
		t.Fatal("the first health check did not start")
	}

	// The subscription content changes while that check is still running.
	fresh := []C.Proxy{&reportingProxy{Proxy: adapter.NewProxy(outbound.NewDirect()), name: "new", tested: tested}}
	bp.setProxies(fresh)

	select {
	case name := <-tested:
		require.Equal(t, "new", name)
	case <-time.After(3 * time.Second):
		t.Fatal("proxies set during a running health check were not tested")
	}
}
