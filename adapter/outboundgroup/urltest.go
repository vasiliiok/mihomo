package outboundgroup

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/metacubex/mihomo/common/callback"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/singledo"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"

	"golang.org/x/exp/slices"
)

type URLTestOption struct {
	Tolerance    uint16 `group:"tolerance,omitempty"`
	Reliability  int    `group:"reliability,omitempty"`
	DelaySamples int    `group:"delay-samples,omitempty"`
}

type URLTest struct {
	*GroupBase
	selected       string
	testUrl        string
	expectedStatus string
	tolerance      uint16
	reliability    int // percent of successful recent checks a proxy needs to be preferred, 0 = off
	delaySamples   int // a proxy is compared by the median of its last delaySamples delays, 0 or 1 = the last one
	disableUDP     bool
	fastNode       C.Proxy
	fastSingle     *singledo.Single[C.Proxy]
}

func (u *URLTest) Now() string {
	return u.fast(false).Name()
}

func (u *URLTest) Set(name string) error {
	var p C.Proxy
	for _, proxy := range u.GetProxies(false) {
		if proxy.Name() == name {
			p = proxy
			break
		}
	}
	if p == nil {
		return errors.New("proxy not exist")
	}
	u.ForceSet(name)
	return nil
}

func (u *URLTest) ForceSet(name string) {
	u.selected = name
	u.fastSingle.Reset()
}

// DialContext implements C.ProxyAdapter
func (u *URLTest) DialContext(ctx context.Context, metadata *C.Metadata) (c C.Conn, err error) {
	proxy := u.fast(true)
	c, err = proxy.DialContext(ctx, metadata)
	if err == nil {
		c.AppendToChains(u)
	} else {
		u.onDialFailed(proxy.Type(), err, u.healthCheck)
	}

	if N.NeedHandshake(c) {
		c = callback.NewFirstWriteCallBackConn(c, func(err error) {
			u.diagDialed(ctx, err)
			if err == nil {
				u.onDialSuccess()
			} else {
				u.onDialFailed(proxy.Type(), err, u.healthCheck)
			}
		})
	} else {
		u.diagDialed(ctx, err)
	}

	return c, err
}

// ListenPacketContext implements C.ProxyAdapter
func (u *URLTest) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	proxy := u.fast(true)
	pc, err := proxy.ListenPacketContext(ctx, metadata)
	u.diagDialed(ctx, err)
	if err == nil {
		pc.AppendToChains(u)
	} else {
		u.onDialFailed(proxy.Type(), err, u.healthCheck)
	}

	return pc, err
}

// Unwrap implements C.ProxyAdapter
func (u *URLTest) Unwrap(metadata *C.Metadata, touch bool) C.Proxy {
	return u.fast(touch)
}

func (u *URLTest) healthCheck() {
	u.fastSingle.Reset()
	u.GroupBase.healthCheck()
	u.fastSingle.Reset()
}

func (u *URLTest) fast(touch bool) C.Proxy {
	elm, _, shared := u.fastSingle.Do(func() (C.Proxy, error) {
		proxies := u.GetProxies(touch)
		if u.selected != "" {
			for _, proxy := range proxies {
				if !proxy.AliveForTestUrl(u.testUrl) {
					continue
				}
				if proxy.Name() == u.selected {
					u.fastNode = proxy
					return u.diagPick(proxy, "fixed", nil, nil, rating{}), nil
				}
			}
		}

		fast := proxies[0]
		fastRating := u.rate(fast)
		var current C.Proxy

		for i, proxy := range proxies {
			// A provider recreates its proxy objects whenever the fetched content
			// changes, so the stored fastNode may be a stale object that no health
			// check updates any more. Look it up by name to get the current object.
			// The first proxy is included, otherwise tolerance never applies to it.
			if current == nil && u.fastNode != nil && proxy.Name() == u.fastNode.Name() {
				current = proxy
			}

			if i == 0 || !proxy.AliveForTestUrl(u.testUrl) {
				continue
			}

			if r := u.rate(proxy); r.better(fastRating) {
				fast, fastRating = proxy, r
			}
		}

		// tolerance
		if current == nil || !current.AliveForTestUrl(u.testUrl) {
			reason := u.diagUnavailable(current)
			u.fastNode = fast
			return u.diagPick(fast, reason, current, fast, fastRating), nil
		}

		// A recreated proxy has no result for testUrl until the health check reaches it,
		// and its delay reads as the maximum meanwhile. Until then judge it by the last
		// result of its previous object, instead of jumping to whichever proxy happens
		// to be tested first.
		judged := current
		if !hasTestResult(current, u.testUrl) && hasTestResult(u.fastNode, u.testUrl) {
			judged = u.fastNode
		}

		// int, not uint16: a missing delay (0xffff) plus tolerance would wrap around
		judgedRating := u.rate(judged)
		switch {
		case !judgedRating.alive:
			u.fastNode = fast
			return u.diagPick(fast, "last-result-dead", judged, fast, fastRating), nil
		case fastRating.reliable && !judgedRating.reliable:
			u.fastNode = fast
			return u.diagPick(fast, "unreliable", judged, fast, fastRating), nil
		case int(judgedRating.delay) > int(fastRating.delay)+int(u.tolerance):
			u.fastNode = fast
			return u.diagPick(fast, "slower", judged, fast, fastRating), nil
		}

		// keep the tested object as the reference until current gets its own result
		u.fastNode = judged
		return u.diagPick(current, diagKeep(judged, current), judged, fast, fastRating), nil
	})
	if shared && touch { // a shared fastSingle.Do() may cause providers untouched, so we touch them again
		u.Touch()
	}

	return elm
}

// hasTestResult reports whether proxy has been health checked against url.
func hasTestResult(proxy C.Proxy, url string) bool {
	_, ok := proxy.ExtraDelayHistories()[url]
	return ok
}

// rating is how url-test compares proxies: alive is the result of the last check,
// reliable whether enough of the recent checks succeeded (ok of n), and delay the
// delay the proxy is compared by.
type rating struct {
	alive    bool
	reliable bool
	delay    uint16
	ok, n    int // successful and all checks in the history, when reliability or delay-samples is set
}

// better reports whether r should be picked over o: an alive proxy beats a dead one,
// a reliable one an unreliable one, and then the lower delay wins.
func (r rating) better(o rating) bool {
	switch {
	case r.alive != o.alive:
		return r.alive
	case r.reliable != o.reliable:
		return r.reliable
	default:
		return r.delay < o.delay
	}
}

// rate rates proxy for testUrl. With neither reliability nor delay-samples set, this
// is the upstream rule: every alive proxy is reliable and the last delay decides.
func (u *URLTest) rate(proxy C.Proxy) rating {
	alive := proxy.AliveForTestUrl(u.testUrl)
	r := rating{alive: alive, reliable: alive, delay: proxy.LastDelayForTestUrl(u.testUrl)}
	if !alive || (u.reliability == 0 && u.delaySamples <= 1) {
		return r
	}
	history := testHistory(proxy, u.testUrl)
	if len(history) == 0 {
		// untested: nothing shows it is reliable yet
		r.reliable = u.reliability == 0
		return r
	}
	samples := make([]uint16, 0, u.delaySamples)
	for i := len(history) - 1; i >= 0; i-- {
		if d := history[i].Delay; d > 0 {
			r.ok++
			if len(samples) < u.delaySamples {
				samples = append(samples, d)
			}
		}
	}
	r.n = len(history)
	r.reliable = r.ok*100 >= u.reliability*r.n
	if len(samples) > 1 {
		// the median: a single slow or fast check does not move the proxy up or down
		slices.Sort(samples)
		r.delay = samples[len(samples)/2]
	}
	return r
}

// testHistory returns the health check history of proxy for url, oldest first.
func testHistory(proxy C.Proxy, url string) []C.DelayHistory {
	if p, ok := proxy.(interface {
		DelayHistoryForTestUrl(string) []C.DelayHistory
	}); ok {
		return p.DelayHistoryForTestUrl(url)
	}
	return proxy.ExtraDelayHistories()[url].History
}

// SupportUDP implements C.ProxyAdapter
func (u *URLTest) SupportUDP() bool {
	if u.disableUDP {
		return false
	}
	return u.fast(false).SupportUDP()
}

// IsL3Protocol implements C.ProxyAdapter
func (u *URLTest) IsL3Protocol(metadata *C.Metadata) bool {
	return u.fast(false).IsL3Protocol(metadata)
}

// MarshalJSON implements C.ProxyAdapter
func (u *URLTest) MarshalJSON() ([]byte, error) {
	all := []string{}
	for _, proxy := range u.GetProxies(false) {
		all = append(all, proxy.Name())
	}
	return json.Marshal(map[string]any{
		"type":           u.Type().String(),
		"now":            u.Now(),
		"all":            all,
		"testUrl":        u.testUrl,
		"expectedStatus": u.expectedStatus,
		"fixed":          u.selected,
		"hidden":         u.Hidden(),
		"icon":           u.Icon(),
		"emptyFallback":  u.EmptyFallback().Name(),
	})
}

func (u *URLTest) Providers() []P.ProxyProvider {
	return u.providers
}

func (u *URLTest) Proxies() []C.Proxy {
	return u.GetProxies(false)
}

func (u *URLTest) URLTest(ctx context.Context, url string, expectedStatus utils.IntRanges[uint16]) (map[string]uint16, error) {
	return u.GroupBase.URLTest(ctx, u.testUrl, expectedStatus)
}

func NewURLTest(option GroupCommonOption, urlTestOption URLTestOption, emptyFallback C.Proxy, providers []P.ProxyProvider) (*URLTest, error) {
	if emptyFallback == nil {
		return nil, errors.New("empty fallback proxy not exist")
	}
	urlTest := &URLTest{
		GroupBase: NewGroupBase(GroupBaseOption{
			Name:           option.Name,
			Type:           C.URLTest,
			Hidden:         option.Hidden,
			Icon:           option.Icon,
			Filter:         option.Filter,
			ExcludeFilter:  option.ExcludeFilter,
			ExcludeType:    option.ExcludeType,
			TestTimeout:    option.TestTimeout,
			MaxFailedTimes: option.MaxFailedTimes,
			EmptyFallback:  emptyFallback,
			Providers:      providers,
		}),
		fastSingle:     singledo.NewSingle[C.Proxy](time.Second * 10),
		disableUDP:     option.DisableUDP,
		testUrl:        option.URL,
		expectedStatus: option.ExpectedStatus,
		tolerance:      urlTestOption.Tolerance,
		reliability:    clamp(urlTestOption.Reliability, 0, 100),
		delaySamples:   clamp(urlTestOption.DelaySamples, 0, 10), // the history keeps 10 checks
	}

	return urlTest, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
