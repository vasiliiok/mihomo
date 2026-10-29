package outboundgroup

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/callback"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

type FallbackOption struct {
	FailConfirm bool `group:"fail-confirm,omitempty"`
}

type Fallback struct {
	*GroupBase
	disableUDP     bool
	testUrl        string
	selected       string
	expectedStatus string
	failConfirm    bool // a member is dead only once a re-check confirms its failed check
}

func (f *Fallback) Now() string {
	proxy := f.findAliveProxy(false)
	return proxy.Name()
}

// DialContext implements C.ProxyAdapter
func (f *Fallback) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	proxy := f.findAliveProxy(true)
	c, err := proxy.DialContext(ctx, metadata)
	f.diagDialed(err)
	if err == nil {
		c.AppendToChains(f)
	} else {
		f.onDialFailed(proxy.Type(), err, f.healthCheck)
	}

	if N.NeedHandshake(c) {
		c = callback.NewFirstWriteCallBackConn(c, func(err error) {
			if err == nil {
				f.onDialSuccess()
			} else {
				f.onDialFailed(proxy.Type(), err, f.healthCheck)
			}
		})
	}

	return c, err
}

// ListenPacketContext implements C.ProxyAdapter
func (f *Fallback) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	proxy := f.findAliveProxy(true)
	pc, err := proxy.ListenPacketContext(ctx, metadata)
	f.diagDialed(err)
	if err == nil {
		pc.AppendToChains(f)
	}

	return pc, err
}

// SupportUDP implements C.ProxyAdapter
func (f *Fallback) SupportUDP() bool {
	if f.disableUDP {
		return false
	}

	proxy := f.findAliveProxy(false)
	return proxy.SupportUDP()
}

// IsL3Protocol implements C.ProxyAdapter
func (f *Fallback) IsL3Protocol(metadata *C.Metadata) bool {
	return f.findAliveProxy(false).IsL3Protocol(metadata)
}

// MarshalJSON implements C.ProxyAdapter
func (f *Fallback) MarshalJSON() ([]byte, error) {
	all := []string{}
	for _, proxy := range f.GetProxies(false) {
		all = append(all, proxy.Name())
	}
	return json.Marshal(map[string]any{
		"type":           f.Type().String(),
		"now":            f.Now(),
		"all":            all,
		"testUrl":        f.testUrl,
		"expectedStatus": f.expectedStatus,
		"fixed":          f.selected,
		"hidden":         f.Hidden(),
		"icon":           f.Icon(),
		"emptyFallback":  f.EmptyFallback().Name(),
	})
}

// Unwrap implements C.ProxyAdapter
func (f *Fallback) Unwrap(metadata *C.Metadata, touch bool) C.Proxy {
	proxy := f.findAliveProxy(touch)
	return proxy
}

func (f *Fallback) findAliveProxy(touch bool) C.Proxy {
	proxies := f.GetProxies(touch)
	for _, proxy := range proxies {
		if len(f.selected) == 0 {
			if f.alive(proxy) {
				return f.diagNote(proxy)
			}
		} else {
			if proxy.Name() == f.selected {
				if f.alive(proxy) {
					return f.diagNote(proxy)
				} else {
					f.selected = ""
				}
			}
		}
	}

	return f.diagNote(proxies[0])
}

// alive reports whether the group may use member p. With fail-confirm, a member whose
// last check failed stays in use until a re-check confirms the failure: most failed
// checks of a working member are a single timeout.
func (f *Fallback) alive(p C.Proxy) bool {
	alive, suspect := f.memberState(p)
	if suspect {
		f.confirm(p)
	}
	return alive
}

// memberState returns whether member p counts as alive, and whether it is only
// suspected to be dead: its last check failed and no re-check has confirmed that yet.
func (f *Fallback) memberState(p C.Proxy) (alive, suspect bool) {
	if p.AliveForTestUrl(f.testUrl) {
		return true, false
	}
	if !f.failConfirm {
		return false, false
	}
	history := testHistory(p, f.testUrl)
	if len(history) == 0 {
		return false, false
	}
	if c, ok := confirmations.Load(confirmKey{p, f.testUrl}); ok {
		if failedAt := c.(*confirmation).lastFailure(); !failedAt.IsZero() && !succeededSince(history, failedAt) {
			return false, false
		}
	}
	return true, true
}

// confirmations holds the re-checks of members that failed a check. It is shared by
// all fallback groups: one member is often in several groups with the same url.
var confirmations sync.Map // confirmKey → *confirmation

type confirmKey struct {
	proxy C.Proxy
	url   string
}

type confirmation struct {
	mu       sync.Mutex
	running  bool
	failedAt time.Time // when the last re-check failed
}

func (c *confirmation) lastFailure() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failedAt
}

// confirm re-checks member p a second after its failed check, unless a re-check is running.
func (f *Fallback) confirm(p C.Proxy) {
	v, _ := confirmations.LoadOrStore(confirmKey{p, f.testUrl}, &confirmation{})
	c := v.(*confirmation)
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()

	var failedCheck time.Time
	if history := testHistory(p, f.testUrl); len(history) > 0 {
		failedCheck = history[len(history)-1].Time
	}
	go func() {
		if d := time.Until(failedCheck.Add(time.Second)); d > 0 {
			time.Sleep(d)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(f.testTimeout)*time.Millisecond)
		defer cancel()
		expectedStatus, _ := utils.NewUnsignedRanges[uint16](f.expectedStatus)
		delay, err := p.URLTest(ctx, f.testUrl, expectedStatus)
		failed := err != nil || !p.AliveForTestUrl(f.testUrl)

		c.mu.Lock()
		if failed {
			c.failedAt = time.Now()
		}
		c.running = false
		c.mu.Unlock()
		f.diagConfirmed(p, failed, delay, err, failedCheck)
	}()
}

// succeededSince reports whether a check in history succeeded after t.
func succeededSince(history []C.DelayHistory, t time.Time) bool {
	for i := len(history) - 1; i >= 0 && history[i].Time.After(t); i-- {
		if history[i].Delay > 0 {
			return true
		}
	}
	return false
}

func (f *Fallback) Set(name string) error {
	var p C.Proxy
	for _, proxy := range f.GetProxies(false) {
		if proxy.Name() == name {
			p = proxy
			break
		}
	}

	if p == nil {
		return errors.New("proxy not exist")
	}

	f.selected = name
	if !p.AliveForTestUrl(f.testUrl) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*time.Duration(5000))
		defer cancel()
		expectedStatus, _ := utils.NewUnsignedRanges[uint16](f.expectedStatus)
		_, _ = p.URLTest(ctx, f.testUrl, expectedStatus)
	}

	return nil
}

func (f *Fallback) ForceSet(name string) {
	f.selected = name
}

func (f *Fallback) Providers() []P.ProxyProvider {
	return f.providers
}

func (f *Fallback) Proxies() []C.Proxy {
	return f.GetProxies(false)
}

func NewFallback(option GroupCommonOption, fallbackOption FallbackOption, emptyFallback C.Proxy, providers []P.ProxyProvider) (*Fallback, error) {
	return &Fallback{
		GroupBase: NewGroupBase(GroupBaseOption{
			Name:           option.Name,
			Type:           C.Fallback,
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
		disableUDP:     option.DisableUDP,
		testUrl:        option.URL,
		expectedStatus: option.ExpectedStatus,
		failConfirm:    fallbackOption.FailConfirm,
	}, nil
}
