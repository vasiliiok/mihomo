// Package diag writes an optional diagnostic log: proxy group decisions,
// health check rounds, provider updates and periodic status lines.
//
// It is off unless MIHOMO_DIAG_DIR is set, and it does not depend on
// log-level. The lines never contain traffic destinations or client
// addresses, and URLs and addresses in error messages are masked, so the
// files can be shared for analysis.
//
//	MIHOMO_DIAG_DIR       directory for diag-YYYY-MM-DD.log files (enables the log)
//	MIHOMO_DIAG_INTERVAL  period of status lines, Go duration, default 1m
//	MIHOMO_DIAG_KEEP_DAYS days of files to keep, default 14
//
// Finished days are compressed to diag-YYYY-MM-DD.log.gz.
package diag

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/log"
)

const (
	envDir      = "MIHOMO_DIAG_DIR"
	envInterval = "MIHOMO_DIAG_INTERVAL"
	envKeep     = "MIHOMO_DIAG_KEEP_DAYS"

	filePrefix = "diag-"
	fileSuffix = ".log"
	dayLayout  = "2006-01-02"
	timeLayout = "2006-01-02T15:04:05.000Z07:00"
)

var (
	enabled  bool
	dir      string
	interval = time.Minute
	keepDays = 14

	mu      sync.Mutex
	file    *os.File
	fileDay string
	failed  bool // the last open failed; reported once

	now = time.Now
)

func init() {
	configure(os.Getenv)
}

func configure(getenv func(string) string) {
	dir = strings.TrimSpace(getenv(envDir))
	enabled = dir != ""
	if d, err := time.ParseDuration(strings.TrimSpace(getenv(envInterval))); err == nil && d >= time.Second {
		interval = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(envKeep))); err == nil && n > 0 {
		keepDays = n
	}
}

// SetDir turns the diagnostic log on in d, or off for "". For tests.
func SetDir(d string) {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file, fileDay = nil, ""
	}
	dir, enabled = d, d != ""
}

// Enabled reports whether the diagnostic log is on.
func Enabled() bool {
	return enabled
}

// Interval is the period of status lines.
func Interval() time.Duration {
	return interval
}

// Dir is the directory of the diagnostic log files.
func Dir() string {
	return dir
}

// Log writes one line "<time> <event> <fields>".
func Log(event, format string, args ...any) {
	if !enabled {
		return
	}
	t := now()
	line := t.Format(timeLayout) + " " + event + " " + fmt.Sprintf(format, args...) + "\n"

	mu.Lock()
	defer mu.Unlock()
	if err := openFor(t); err != nil {
		if !failed {
			failed = true
			log.Errorln("[Diag] cannot write the diagnostic log to %s: %v", dir, err)
		}
		return
	}
	failed = false
	_, _ = file.WriteString(line)
}

// Warn writes a "warn" line and repeats the message in the main log.
func Warn(format string, args ...any) {
	if !enabled {
		return
	}
	msg := fmt.Sprintf(format, args...)
	Log("warn", "%s", msg)
	log.Warnln("[Diag] %s", msg)
}

// openFor makes sure file is the file of t's day. Must be called with mu held.
func openFor(t time.Time) error {
	day := t.Format(dayLayout)
	if file != nil && day == fileDay {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, filePrefix+day+fileSuffix), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if file != nil {
		_ = file.Close()
	}
	file, fileDay = f, day
	tidying.Add(1)
	go tidy(dir, day, t)
	return nil
}

var (
	tidyMu  sync.Mutex
	tidying sync.WaitGroup
)

// tidy compresses the files of finished days and removes files older than keepDays.
func tidy(dir, today string, t time.Time) {
	defer tidying.Done()
	tidyMu.Lock()
	defer tidyMu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	oldest := t.AddDate(0, 0, -keepDays).Format(dayLayout)
	for _, e := range entries {
		name := e.Name()
		day, ok := dayOf(name)
		if !ok || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		switch {
		case day < oldest:
			_ = os.Remove(path)
		case day < today && strings.HasSuffix(name, fileSuffix):
			if err := compress(path); err != nil {
				log.Warnln("[Diag] compress %s: %v", path, err)
			}
		}
	}
}

func dayOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, filePrefix)
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(strings.TrimSuffix(rest, ".gz"), fileSuffix)
	if !ok {
		return "", false
	}
	if _, err := time.Parse(dayLayout, rest); err != nil {
		return "", false
	}
	return rest, true
}

func compress(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := path + ".gz.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	err = errors.Join(err, zw.Close(), out.Close())
	if err == nil {
		err = os.Rename(tmp, path+".gz")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(path)
}

var (
	urlRe  = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']+`)
	ipRe   = regexp.MustCompile(`\[[0-9a-fA-F:.]+\](:\d+)?|\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)
	hostRe = regexp.MustCompile(`\b[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)*\.[a-zA-Z]{2,}:\d+\b`)
)

// Err returns err as a short string with URLs, IP addresses and host:port masked.
func Err(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = urlRe.ReplaceAllString(s, "<url>")
	s = ipRe.ReplaceAllString(s, "<ip>")
	s = hostRe.ReplaceAllString(s, "<host>")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// Delay formats a health check delay, "-" for none.
func Delay(d uint16) string {
	if d == 0 || d == 0xffff {
		return "-"
	}
	return strconv.Itoa(int(d))
}

// Names formats a list of names, at most limit of them.
func Names(names []string, limit int) string {
	sort.Strings(names)
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range names {
		if i == limit {
			fmt.Fprintf(&b, " …+%d", len(names)-limit)
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Quote(n))
	}
	b.WriteByte(']')
	return b.String()
}
