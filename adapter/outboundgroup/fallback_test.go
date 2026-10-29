package outboundgroup

import (
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"

	"github.com/stretchr/testify/require"
)

func newTestFallback(t *testing.T, failConfirm bool, members ...C.Proxy) *Fallback {
	t.Helper()
	pd := &fakeProxyProvider{}
	pd.update(members)
	group, err := NewFallback(GroupCommonOption{Name: "fallback", URL: testUrl, TestTimeout: 1000},
		FallbackOption{FailConfirm: failConfirm}, adapter.NewProxy(outbound.NewDirect()), []P.ProxyProvider{pd})
	require.NoError(t, err)
	return group
}

func TestFallbackLeavesMemberOnSingleFailureByDefault(t *testing.T) {
	main, spare := historyProxy("main", 80), historyProxy("spare", 90)
	group := newTestFallback(t, false, main, spare)
	require.Same(t, spare, func() C.Proxy { main.check(0); return group.findAliveProxy(false) }())
	require.Zero(t, main.testCount(), "a re-check ran without fail-confirm")
}

func TestFallbackFailConfirmKeepsMemberAfterTransientFailure(t *testing.T) {
	main, spare := historyProxy("main", 80), historyProxy("spare", 90)
	main.results = []uint16{85} // the re-check succeeds
	group := newTestFallback(t, true, main, spare)

	main.check(0)
	start := time.Now()
	require.Same(t, main, group.findAliveProxy(false), "a single failed check moved the group")
	require.Eventually(t, func() bool { return main.AliveForTestUrl(testUrl) }, 3*time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "the re-check did not wait a second")
	require.Same(t, main, group.findAliveProxy(false))
	require.Equal(t, 1, main.testCount())
}

func TestFallbackFailConfirmLeavesMemberThatFailsTheRecheck(t *testing.T) {
	main, spare := historyProxy("main", 80), historyProxy("spare", 90)
	main.results = []uint16{0} // the re-check fails as well
	group := newTestFallback(t, true, main, spare)

	main.check(0)
	require.Same(t, main, group.findAliveProxy(false))
	require.Eventually(t, func() bool { return group.findAliveProxy(false) == C.Proxy(spare) }, 3*time.Second, 10*time.Millisecond,
		"the group stayed on a member whose failure was confirmed")

	// still failing: no new re-checks, the member stays dead
	main.check(0)
	for i := 0; i < 5; i++ {
		require.Same(t, spare, group.findAliveProxy(false))
	}
	require.Equal(t, 1, main.testCount(), "a confirmed dead member was re-checked again")

	// it recovers, then fails once more: suspected again, not dead
	main.check(70)
	require.Same(t, main, group.findAliveProxy(false))
	main.results = []uint16{75}
	main.check(0)
	require.Same(t, main, group.findAliveProxy(false), "a recovered member was left on a single failure")
	require.Eventually(t, func() bool { return main.testCount() == 2 }, 3*time.Second, 10*time.Millisecond)
}

func TestFallbackFailConfirmRunsOneRecheckForSharedMember(t *testing.T) {
	shared, spare := historyProxy("shared", 80), historyProxy("spare", 90)
	shared.results = []uint16{85}
	a := newTestFallback(t, true, shared, spare)
	b := newTestFallback(t, true, shared, spare)

	shared.check(0)
	for i := 0; i < 10; i++ {
		require.Same(t, shared, a.findAliveProxy(false))
		require.Same(t, shared, b.findAliveProxy(false))
	}
	require.Eventually(t, func() bool { return shared.AliveForTestUrl(testUrl) }, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, shared.testCount(), "groups sharing a member re-checked it separately")
}

func TestFallbackFailConfirmKeepsFixedMember(t *testing.T) {
	main, spare := historyProxy("main", 80), historyProxy("spare", 90)
	spare.results = []uint16{95}
	group := newTestFallback(t, true, main, spare)
	group.ForceSet("spare")
	spare.check(0)
	require.Same(t, spare, group.findAliveProxy(false))
	require.Equal(t, "spare", group.selected, "a single failed check unfixed the member")
	require.Eventually(t, func() bool { return spare.AliveForTestUrl(testUrl) }, 3*time.Second, 10*time.Millisecond)
}
