// Package diagreport writes the periodic status and configuration lines of the
// diagnostic log (see component/diag).
package diagreport

import (
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/diag"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

type statusReporter interface {
	DiagStatus() string
}

type configReporter interface {
	DiagConfig() string
}

var (
	once    sync.Once
	started = time.Now()
)

// ConfigApplied logs the applied configuration and starts the status lines.
func ConfigApplied() {
	if !diag.Enabled() {
		return
	}
	once.Do(func() {
		log.Infoln("[Diag] diagnostic log in %s, status every %s", diag.Dir(), diag.Interval())
		diag.Log("start", "version=%s build=%q go=%s pid=%d interval=%s", C.Version, C.BuildTime, runtime.Version(), os.Getpid(), diag.Interval())
		go loop()
	})
	logConfig()
}

func loop() {
	ticker := time.NewTicker(diag.Interval())
	defer ticker.Stop()
	for range ticker.C {
		report()
	}
}

// groups returns the proxy groups sorted: fallbacks first, then by name.
func groups() []C.Proxy {
	var list []C.Proxy
	for _, p := range tunnel.Proxies() {
		if _, ok := p.Adapter().(statusReporter); ok {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		fi, fj := list[i].Type() == C.Fallback, list[j].Type() == C.Fallback
		if fi != fj {
			return fi
		}
		return list[i].Name() < list[j].Name()
	})
	return list
}

func logConfig() {
	for _, p := range groups() {
		if c, ok := p.Adapter().(configReporter); ok {
			diag.Log("config", "%s", c.DiagConfig())
		}
	}
	providers := tunnel.Providers()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pd := providers[name]
		desc := ""
		if c, ok := pd.(configReporter); ok {
			desc = c.DiagConfig()
		}
		diag.Log("config", "provider=%q vehicle=%s %s", name, pd.VehicleType(), desc)
	}
}

func report() {
	defer func() {
		if r := recover(); r != nil { // a status line must never take mihomo down
			diag.Log("error", "status report panicked: %v", r)
		}
	}()
	for _, p := range groups() {
		diag.Log("status", "%s", p.Adapter().(statusReporter).DiagStatus())
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	conns := 0
	statistic.DefaultManager.Range(func(statistic.Tracker) bool {
		conns++
		return true
	})
	up, down := statistic.DefaultManager.Total()
	diag.Log("runtime", "uptime=%s goroutines=%d heap=%dMiB sys=%dMiB rss=%dMiB gc=%d conns=%d up=%dMiB down=%dMiB",
		time.Since(started).Round(time.Second), runtime.NumGoroutine(), ms.HeapAlloc>>20, ms.Sys>>20,
		statistic.DefaultManager.Memory()>>20, ms.NumGC, conns, up>>20, down>>20)
}
