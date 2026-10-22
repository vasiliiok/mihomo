package provider

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/component/diag"
	C "github.com/metacubex/mihomo/constant"
)

// hcRound records one health check round in the diagnostic log; nil when it is off.
type hcRound struct {
	provider string
	id       string
	reason   string
	url      string // the default url, not repeated in node lines
	start    time.Time
	tested   atomic.Int32
	alive    atomic.Int32
}

func newHCRound(hc *HealthCheck, id, reason string, proxies []C.Proxy, version uint64) *hcRound {
	if !diag.Enabled() {
		return nil
	}
	r := &hcRound{provider: hc.name, id: id[:8], reason: reason, url: hc.url, start: time.Now()}
	extra := make([]string, 0, len(hc.extra))
	for url := range hc.extra {
		extra = append(extra, url)
	}
	diag.Log("hc-start", "provider=%q round=%s reason=%s proxies=%d version=%d url=%s extra=%s",
		r.provider, r.id, reason, len(proxies), version, hc.url, diag.Names(extra, 10))
	return r
}

func (r *hcRound) node(url string, proxy C.Proxy, err error, full time.Duration) {
	if r == nil {
		return
	}
	alive := proxy.AliveForTestUrl(url)
	r.tested.Add(1)
	if alive {
		r.alive.Add(1)
	}
	at := ""
	if url != r.url {
		at = " url=" + url
	}
	msg := ""
	if err != nil {
		msg = fmt.Sprintf(" err=%q", diag.Err(err))
	}
	diag.Log("hc-node", "provider=%q round=%s node=%q ok=%t delay=%s full=%d%s%s",
		r.provider, r.id, proxy.Name(), alive, diag.Delay(proxy.LastDelayForTestUrl(url)), full.Milliseconds(), at, msg)
}

func (r *hcRound) finish() {
	if r == nil {
		return
	}
	diag.Log("hc-end", "provider=%q round=%s reason=%s tests=%d alive=%d dur=%d",
		r.provider, r.id, r.reason, r.tested.Load(), r.alive.Load(), time.Since(r.start).Milliseconds())
}

func diagSkippedCheck(hc *HealthCheck, reason string) {
	diag.Log("hc-skip", "provider=%q reason=%s (joined a running round or reused one that ended <1s ago)", hc.name, reason)
}

func diagRecheck(hc *HealthCheck) {
	diag.Log("hc-recheck", "provider=%q (proxies replaced since the last round started: new round)", hc.name)
}

// diagSetProxies logs a provider update; called with the old list still in place.
func diagSetProxies(name string, version uint32, old, proxies []C.Proxy) {
	if !diag.Enabled() {
		return
	}
	if len(old) == 0 {
		diag.Log("provider-update", "provider=%q version=%d proxies=%d initial", name, version, len(proxies))
		return
	}
	before := make(map[string]struct{}, len(old))
	for _, p := range old {
		before[p.Name()] = struct{}{}
	}
	var added []string
	now := make(map[string]struct{}, len(proxies))
	for _, p := range proxies {
		now[p.Name()] = struct{}{}
		if _, ok := before[p.Name()]; !ok {
			added = append(added, p.Name())
		}
	}
	var removed []string
	for n := range before {
		if _, ok := now[n]; !ok {
			removed = append(removed, n)
		}
	}
	diag.Log("provider-update", "provider=%q version=%d proxies=%d (all recreated) added=%d removed=%d %s %s",
		name, version, len(proxies), len(added), len(removed),
		"added:"+diag.Names(added, 30), "removed:"+diag.Names(removed, 30))
}

// DiagConfig describes the health check settings for the diagnostic log.
func (bp *baseProvider) DiagConfig() string {
	hc := bp.healthCheck
	hc.mu.Lock()
	defer hc.mu.Unlock()
	extra := make([]string, 0, len(hc.extra))
	for url, opt := range hc.extra {
		filters := make([]string, 0, len(opt.filters))
		for f := range opt.filters {
			filters = append(filters, f)
		}
		extra = append(extra, url+" filter="+strings.Join(filters, "|"))
	}
	return fmt.Sprintf("hc_url=%s interval=%s timeout=%s lazy=%t expected=%q extra=%s proxies=%d",
		hc.url, hc.interval, hc.timeout, hc.lazy, hc.expectedStatus.String(), diag.Names(extra, 10), len(hc.proxies))
}
