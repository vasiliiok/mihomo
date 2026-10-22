package outboundgroup

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/component/diag"
	C "github.com/metacubex/mihomo/constant"
)

// diagState is what a group remembers for the diagnostic log.
type diagState struct {
	mu       sync.Mutex
	last     string    // name of the proxy picked last time
	reason   string    // why (url-test)
	refDelay uint16    // delay of the object the pick was judged by (url-test)
	at       time.Time // when it was picked (url-test)

	dialOK   atomic.Int64 // since the previous status line
	dialFail atomic.Int64

	maxFailedAt         time.Time // last max-failed line
	maxFailedSuppressed int       // max-failed events not logged since then
}

func (gb *GroupBase) diagDialed(err error) {
	if err == nil && diag.Enabled() {
		gb.diagSt.dialOK.Add(1)
	}
}

func (gb *GroupBase) diagDialFailed() {
	if diag.Enabled() {
		gb.diagSt.dialFail.Add(1)
	}
}

// diagMaxFailed logs that max-failed-times triggered a health check, at most once in 10 s
// per group: with "connection refused" it triggers on every failed connection.
func (gb *GroupBase) diagMaxFailed(why string, failures int) {
	if !diag.Enabled() {
		return
	}
	st := &gb.diagSt
	st.mu.Lock()
	if time.Since(st.maxFailedAt) < 10*time.Second {
		st.maxFailedSuppressed++
		st.mu.Unlock()
		return
	}
	suppressed := st.maxFailedSuppressed
	st.maxFailedAt, st.maxFailedSuppressed = time.Now(), 0
	st.mu.Unlock()
	diag.Log("max-failed", "group=%q failures=%d window=%dms %s: health check of the group's providers (%d more in the previous 10 s+ not logged)",
		gb.Name(), failures, gb.testTimeout, why, suppressed)
}

func (gb *GroupBase) diagDials() string {
	return fmt.Sprintf("dial_ok=%d dial_fail=%d", gb.diagSt.dialOK.Swap(0), gb.diagSt.dialFail.Swap(0))
}

func (gb *GroupBase) diagProviders() string {
	names := make([]string, 0, len(gb.providers))
	for _, pd := range gb.providers {
		names = append(names, pd.Name())
	}
	return diag.Names(names, 20)
}

// diagUnavailable says why url-test cannot keep its node.
func diagUnavailable(prev, current C.Proxy) string {
	switch {
	case prev == nil:
		return "first"
	case current == nil:
		return "removed"
	default:
		return "dead"
	}
}

// diagSlower says why url-test leaves a node that was judged by ref.
func diagSlower(ref C.Proxy, testUrl string) string {
	if !ref.AliveForTestUrl(testUrl) {
		return "last-result-dead"
	}
	return "slower"
}

// diagKeep says whether url-test keeps its node on its own result or on the previous object's.
func diagKeep(ref, current C.Proxy) string {
	if ref != current {
		return "pending"
	}
	return "keep"
}

// diagPick logs a url-test decision when the node or the pending state changes,
// and returns picked. ref is the object the old node was judged by, best the fastest one.
func (u *URLTest) diagPick(picked C.Proxy, reason string, ref, best C.Proxy, minDelay uint16) C.Proxy {
	if !diag.Enabled() {
		return picked
	}
	st := &u.diagSt
	st.mu.Lock()
	defer st.mu.Unlock()
	from, fromReason := st.last, st.reason
	st.last, st.reason, st.at = picked.Name(), reason, time.Now()
	st.refDelay = 0
	if reason == "pending" && ref != nil {
		st.refDelay = ref.LastDelayForTestUrl(u.testUrl)
	}
	if from == picked.Name() && (fromReason == "pending") == (reason == "pending") {
		return picked
	}
	refDelay, bestName := "-", ""
	if ref != nil {
		refDelay = diag.Delay(ref.LastDelayForTestUrl(u.testUrl))
	}
	if best != nil {
		bestName = best.Name()
	}
	diag.Log("url-test", "group=%q from=%q to=%q reason=%s from_delay=%s to_delay=%s best=%q best_delay=%s tol=%d",
		u.Name(), from, picked.Name(), reason, refDelay, diag.Delay(picked.LastDelayForTestUrl(u.testUrl)),
		bestName, diag.Delay(minDelay), u.tolerance)
	return picked
}

// DiagStatus returns the status line of the group for the diagnostic log.
func (u *URLTest) DiagStatus() string {
	now := u.fast(false)
	var alive, dead, untested int
	var best C.Proxy
	bestDelay := 0xffff
	proxies := u.GetProxies(false)
	for _, p := range proxies {
		switch {
		case !hasTestResult(p, u.testUrl):
			untested++
		case p.AliveForTestUrl(u.testUrl):
			alive++
			if d := int(p.LastDelayForTestUrl(u.testUrl)); d < bestDelay {
				best, bestDelay = p, d
			}
		default:
			dead++
		}
	}

	u.diagSt.mu.Lock()
	reason, refDelay, at := u.diagSt.reason, u.diagSt.refDelay, u.diagSt.at
	u.diagSt.mu.Unlock()

	nowDelay := int(now.LastDelayForTestUrl(u.testUrl))
	extra := ""
	if reason == "pending" && !hasTestResult(now, u.testUrl) {
		extra = " pending_prev_delay=" + diag.Delay(refDelay)
	}
	bestName, delta := "", "-"
	if best != nil {
		bestName = best.Name()
		if nowDelay < 0xffff {
			d := nowDelay - bestDelay
			delta = strconv.Itoa(d)
			if d > int(u.tolerance) && u.selected == "" {
				diag.Warn("url-test %s: %q is %d ms slower than %q (%d vs %d ms, tolerance %d)",
					u.Name(), now.Name(), d, bestName, nowDelay, bestDelay, u.tolerance)
			}
		}
	}
	decided := "-"
	if !at.IsZero() {
		decided = strconv.Itoa(int(time.Since(at).Seconds())) + "s"
	}
	return fmt.Sprintf("url-test=%q now=%q delay=%s best=%q best_delay=%s delta=%s tol=%d alive=%d dead=%d untested=%d total=%d fixed=%q decided=%s reason=%s %s%s",
		u.Name(), now.Name(), diag.Delay(uint16(nowDelay)), bestName, diag.Delay(uint16(bestDelay)), delta, u.tolerance,
		alive, dead, untested, len(proxies), u.selected, decided, reason, u.diagDials(), extra)
}

// DiagConfig describes the group for the diagnostic log.
func (u *URLTest) DiagConfig() string {
	return fmt.Sprintf("url-test=%q url=%s expected=%q tolerance=%d max_failed=%d timeout=%d providers=%s members=%d",
		u.Name(), u.testUrl, u.expectedStatus, u.tolerance, u.maxFailedTimes, u.testTimeout, u.diagProviders(), len(u.GetProxies(false)))
}

// diagNote logs a fallback switch and returns picked.
func (f *Fallback) diagNote(picked C.Proxy) C.Proxy {
	if !diag.Enabled() {
		return picked
	}
	st := &f.diagSt
	st.mu.Lock()
	from := st.last
	changed := from != picked.Name()
	st.last = picked.Name()
	st.mu.Unlock()
	if changed {
		diag.Log("fallback", "group=%q from=%q to=%q fixed=%q members=%s",
			f.Name(), from, picked.Name(), f.selected, diagMembers(f.GetProxies(false), f.testUrl))
	}
	return picked
}

// diagMembers lists the members with their state for testUrl.
func diagMembers(proxies []C.Proxy, testUrl string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, p := range proxies {
		if i > 0 {
			b.WriteByte(' ')
		}
		state := "dead"
		switch {
		case !hasTestResult(p, testUrl):
			state = "untested"
		case p.AliveForTestUrl(testUrl):
			state = diag.Delay(p.LastDelayForTestUrl(testUrl))
		}
		b.WriteString(strconv.Quote(p.Name()) + ":" + state)
	}
	b.WriteByte(']')
	return b.String()
}

// diagChain follows the picks of nested groups: "A → B → node", and returns the
// delay of the last proxy as measured by the group that picked it.
func diagChain(p C.Proxy, testUrl string) (string, string) {
	names := []string{p.Name()}
	for i := 0; i < 8; i++ {
		var next C.Proxy
		url := testUrl
		switch g := p.Adapter().(type) {
		case *URLTest:
			next, url = g.fast(false), g.testUrl
		case *Fallback:
			next, url = g.findAliveProxy(false), g.testUrl
		case *Selector:
			next = g.selectedProxy(false)
		}
		if next == nil {
			break
		}
		p, testUrl = next, url
		names = append(names, p.Name())
	}
	return strconv.Quote(strings.Join(names, " → ")), diag.Delay(p.LastDelayForTestUrl(testUrl))
}

// DiagStatus returns the status line of the group for the diagnostic log.
func (f *Fallback) DiagStatus() string {
	now := f.findAliveProxy(false)
	chain, leafDelay := diagChain(now, f.testUrl)
	return fmt.Sprintf("fallback=%q now=%q chain=%s leaf_delay=%s fixed=%q members=%s %s",
		f.Name(), now.Name(), chain, leafDelay, f.selected, diagMembers(f.GetProxies(false), f.testUrl), f.diagDials())
}

// DiagConfig describes the group for the diagnostic log.
func (f *Fallback) DiagConfig() string {
	names := []string{}
	for _, p := range f.GetProxies(false) {
		names = append(names, strconv.Quote(p.Name()))
	}
	return fmt.Sprintf("fallback=%q url=%s expected=%q max_failed=%d timeout=%d providers=%s members=[%s]",
		f.Name(), f.testUrl, f.expectedStatus, f.maxFailedTimes, f.testTimeout, f.diagProviders(), strings.Join(names, " "))
}
