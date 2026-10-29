package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/metacubex/mihomo/adapter/outbound"

	"github.com/stretchr/testify/require"
)

func TestInheritHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	url := srv.URL + "/generate_204"

	old := NewProxy(outbound.NewDirect())
	_, err := old.URLTest(context.Background(), url, nil)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = old.URLTest(canceled, url, nil) // a failed check
	require.Error(t, err)

	p := NewProxy(outbound.NewDirect())
	p.InheritHealth(old)
	require.Equal(t, old.DelayHistory(), p.DelayHistory())
	require.Equal(t, old.ExtraDelayHistories(), p.ExtraDelayHistories())
	require.False(t, p.AliveForTestUrl(url), "the last inherited check failed")

	// the inherited history is a copy: new checks do not change the old object
	_, err = p.URLTest(context.Background(), url, nil)
	require.NoError(t, err)
	require.True(t, p.AliveForTestUrl(url))
	require.Len(t, p.DelayHistoryForTestUrl(url), 3)
	require.Len(t, old.DelayHistoryForTestUrl(url), 2)
	require.False(t, old.AliveForTestUrl(url))
}
