package diag

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func useClock(t *testing.T, at *time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return *at }
	t.Cleanup(func() {
		now = old
		SetDir("")
	})
}

func TestDisabledWritesNothing(t *testing.T) {
	d := t.TempDir()
	SetDir("")
	Log("x", "y")
	entries, err := os.ReadDir(d)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.False(t, Enabled())
}

func TestLogRotatesCompressesAndPrunes(t *testing.T) {
	d := filepath.Join(t.TempDir(), "diag")
	at := time.Date(2026, 10, 1, 23, 59, 59, 0, time.Local)
	useClock(t, &at)
	SetDir(d)

	// a file older than keepDays and a stray file that must stay
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "diag-2026-09-01.log.gz"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(d, "notes.txt"), nil, 0o644))

	Log("first", "n=%d", 1)
	at = at.Add(2 * time.Second) // next day
	Log("second", "n=%d", 2)
	tidying.Wait()

	_, err := os.Stat(filepath.Join(d, "diag-2026-09-01.log.gz"))
	require.True(t, os.IsNotExist(err), "a file older than keepDays was not removed")
	_, err = os.Stat(filepath.Join(d, "notes.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(d, "diag-2026-10-01.log"))
	require.True(t, os.IsNotExist(err), "the finished day was not compressed")

	f, err := os.Open(filepath.Join(d, "diag-2026-10-01.log.gz"))
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	old, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.Equal(t, at.Add(-2*time.Second).Format(timeLayout)+" first n=1\n", string(old))

	today, err := os.ReadFile(filepath.Join(d, "diag-2026-10-02.log"))
	require.NoError(t, err)
	require.Equal(t, at.Format(timeLayout)+" second n=2\n", string(today))
}

func TestErrMasksAddresses(t *testing.T) {
	err := errors.New(`Get "https://sub.example.com/api/v1/client?token=SECRET": dial tcp 203.0.113.7:443: i/o timeout; [2001:db8::1]:8443 node.example.net:2053`)
	got := Err(err)
	for _, secret := range []string{"SECRET", "sub.example.com", "203.0.113.7", "2001:db8", "node.example.net"} {
		require.NotContains(t, got, secret)
	}
	require.True(t, strings.Contains(got, "i/o timeout"), got)
	require.Equal(t, "", Err(nil))
}

func TestConfigure(t *testing.T) {
	t.Cleanup(func() { configure(func(string) string { return "" }); SetDir("") })
	configure(func(k string) string {
		return map[string]string{envDir: " /var/log/x ", envInterval: "30s", envKeep: "3"}[k]
	})
	require.True(t, Enabled())
	require.Equal(t, "/var/log/x", Dir())
	require.Equal(t, 30*time.Second, Interval())
	require.Equal(t, 3, keepDays)
	keepDays = 14
	interval = time.Minute
}
