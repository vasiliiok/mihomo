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
func (u *URLTest) diagUnavailable(current C.Proxy) string {
	switch {
	case u.fastNode == nil:
		return "first"
	case u.fastNode == u.EmptyFallback():
		return "loaded" // the providers had no proxies before
	case current == nil:
		return "removed"
	default:
		return "dead"
	}
}

// diagKeep says whether url-test keeps its node on its own result or on the previous object's.
func diagKeep(ref, current C.Proxy) string {
	if ref != current {
		return "pending"
	}
	return "keep"
}

// diagPick logs a url-test decision when the node or the pending state changes,
// and returns picked. ref is the object the old node was judged by, best the best one.
func (u *URLTest) diagPick(picked C.Proxy, reason string, ref, best C.Proxy, bestRating rating) C.Proxy {
	if !diag.Enabled() {
		return picked
	}
	var refRating rating
	if ref != nil {
		refRating = u.rate(ref)
	}
	st := &u.diagSt
	st.mu.Lock()
	defer st.mu.Unlock()
	from, fromReason := st.last, st.reason
	st.last, st.reason, st.at = picked.Name(), reason, time.Now()
	st.refDelay = 0
	if reason == "pending" && ref != nil {
		st.refDelay = refRating.delay
	}
	if from == picked.Name() && (fromReason == "pending") == (reason == "pending") {
		return picked
	}
	refDelay, bestName := "-", ""
	if ref != nil {
		refDelay = diag.Delay(refRating.delay)
	}
	if best != nil {
		bestName = best.Name()
	}
	pickedRating := u.rate(picked)
	checks := ""
	if u.rated() {
		checks = fmt.Sprintf(" from_ok=%s to_ok=%s best_ok=%s from_last=%s", refRating.checks(ref != nil), pickedRating.checks(true),
			bestRating.checks(best != nil), diag.Delay(lastDelay(ref, u.testUrl)))
	}
	diag.Log("url-test", "group=%q from=%q to=%q reason=%s from_delay=%s to_delay=%s best=%q best_delay=%s tol=%d%s",
		u.Name(), from, picked.Name(), reason, refDelay, diag.Delay(pickedRating.delay),
		bestName, diag.Delay(bestRating.delay), u.tolerance, checks)
	return picked
}

// rated reports whether url-test looks beyond the last check (reliability or delay-samples).
func (u *URLTest) rated() bool {
	return u.reliability > 0 || u.delaySamples > 1
}

// checks formats the share of successful checks: "9/10", "-" when unknown.
func (r rating) checks(known bool) string {
	if !known || r.n == 0 {
		return "-"
	}
	return strconv.Itoa(r.ok) + "/" + strconv.Itoa(r.n)
}

func lastDelay(p C.Proxy, testUrl string) uint16 {
	if p == nil {
		return 0xffff
	}
	return p.LastDelayForTestUrl(testUrl)
}

// DiagStatus returns the status line of the group for the diagnostic log.
func (u *URLTest) DiagStatus() string {
	now := u.fast(false)
	var alive, dead, untested, unreliable int
	var best C.Proxy
	var bestRating rating
	stale := true // now is an object the provider has replaced (url-test caches its pick for 10 s)
	proxies := u.GetProxies(false)
	for _, p := range proxies {
		if p == now {
			stale = false
		}
		switch {
		case !hasTestResult(p, u.testUrl):
			untested++
		case p.AliveForTestUrl(u.testUrl):
			alive++
			r := u.rate(p)
			if !r.reliable {
				unreliable++
			}
			if best == nil || r.better(bestRating) {
				best, bestRating = p, r
			}
		default:
			dead++
		}
	}

	u.diagSt.mu.Lock()
	reason, refDelay, at := u.diagSt.reason, u.diagSt.refDelay, u.diagSt.at
	u.diagSt.mu.Unlock()

	decided := "-"
	if !at.IsZero() {
		decided = strconv.Itoa(int(time.Since(at).Seconds())) + "s"
	}
	nowRating := u.rate(now)
	nowDelay := int(nowRating.delay)
	extra := ""
	if u.rated() {
		extra = fmt.Sprintf(" ok=%s last=%s best_ok=%s unreliable=%d", nowRating.checks(true),
			diag.Delay(now.LastDelayForTestUrl(u.testUrl)), bestRating.checks(best != nil), unreliable)
	}
	if reason == "pending" && !hasTestResult(now, u.testUrl) {
		extra += " pending_prev_delay=" + diag.Delay(refDelay)
	}
	empty := now == u.EmptyFallback()
	switch {
	case empty:
		// COMPATIBLE is DIRECT: traffic through the group bypasses the proxies
		extra += " empty_fallback=true"
		if best != nil {
			diag.Warn("url-test %s hands out its empty fallback %q while %d of its proxies are alive (decided %s ago)",
				u.Name(), now.Name(), alive, decided)
		}
	case stale:
		extra += " stale_object=true"
	}
	bestName, delta := "", "-"
	bestDelay := 0xffff
	if best != nil {
		bestName, bestDelay = best.Name(), int(bestRating.delay)
		if nowDelay < 0xffff && !stale {
			d := nowDelay - bestDelay
			delta = strconv.Itoa(d)
			if u.selected == "" {
				switch {
				case bestRating.reliable && !nowRating.reliable && hasTestResult(now, u.testUrl):
					diag.Warn("url-test %s: %q is unreliable (%s checks ok, needs %d%%) while %q is reliable (decided %s ago)",
						u.Name(), now.Name(), nowRating.checks(true), u.reliability, bestName, decided)
				case d > int(u.tolerance):
					diag.Warn("url-test %s: %q is %d ms slower than %q (%d vs %d ms, tolerance %d, decided %s ago)",
						u.Name(), now.Name(), d, bestName, nowDelay, bestDelay, u.tolerance, decided)
				}
			}
		}
	}
	return fmt.Sprintf("url-test=%q now=%q delay=%s best=%q best_delay=%s delta=%s tol=%d alive=%d dead=%d untested=%d total=%d fixed=%q decided=%s reason=%s %s%s",
		u.Name(), now.Name(), diag.Delay(uint16(nowDelay)), bestName, diag.Delay(uint16(bestDelay)), delta, u.tolerance,
		alive, dead, untested, len(proxies), u.selected, decided, reason, u.diagDials(), extra)
}

// DiagConfig describes the group for the diagnostic log.
func (u *URLTest) DiagConfig() string {
	return fmt.Sprintf("url-test=%q url=%s expected=%q tolerance=%d reliability=%d delay_samples=%d max_failed=%d timeout=%d providers=%s members=%d",
		u.Name(), u.testUrl, u.expectedStatus, u.tolerance, u.reliability, u.delaySamples, u.maxFailedTimes, u.testTimeout, u.diagProviders(), len(u.GetProxies(false)))
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
