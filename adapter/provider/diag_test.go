package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/diag"
	C "github.com/metacubex/mihomo/constant"

	"github.com/stretchr/testify/require"
)

func TestHealthCheckDiagLogsRounds(t *testing.T) {
	d := t.TempDir()
	diag.SetDir(d)
	defer diag.SetDir("")
	read := func() string {
		files, _ := filepath.Glob(filepath.Join(d, "diag-*.log"))
		var b strings.Builder
		for _, f := range files {
			data, _ := os.ReadFile(f)
			b.Write(data)
		}
		return b.String()
	}

	tested := make(chan string, 16)
	newProxy := func(name string) C.Proxy {
		return &reportingProxy{Proxy: adapter.NewProxy(outbound.NewDirect()), name: name, tested: tested}
	}
	old := []C.Proxy{newProxy("a")}
	hc := NewHealthCheck(old, "https://www.gstatic.com/generate_204", 5000, 300, false, nil)
	defer hc.close()
	hc.name = "sub"
	bp := &baseProvider{name: "sub", proxies: old, healthCheck: hc, version: 1}

	hc.checkFor("tick")
	hc.checkFor("tick") // within a second: reuses the round that has just ended
	bp.setProxies([]C.Proxy{newProxy("b")})

	require.Eventually(t, func() bool { return strings.Count(read(), " hc-end ") == 2 }, 3*time.Second, 10*time.Millisecond, read())
	log := read()
	for _, want := range []string{
		`hc-start provider="sub" round=`,
		`reason=tick proxies=1 version=0 url=https://www.gstatic.com/generate_204 extra=[]`,
		`hc-node provider="sub" round=`,
		`node="a" ok=true delay=- full=`,
		`hc-skip provider="sub" reason=tick`,
		`provider-update provider="sub" version=2 proxies=1 (all recreated) added=1 removed=1 added:["b"] removed:["a"]`,
		`hc-recheck provider="sub"`,
		`reason=update proxies=1 version=1`,
		`node="b" ok=true`,
		`hc-end provider="sub" round=`,
	} {
		require.Contains(t, log, want)
	}
}
