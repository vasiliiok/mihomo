#!/usr/bin/env python3
"""Summarise mihomo diagnostic logs (component/diag).

usage: analyze_diag.py <file or directory or .tar.gz> ...

Reads diag-*.log and diag-*.log.gz (also inside .tar.gz archives made by the
collection command from dist/DIAG.md) and prints a plain-text summary:
restarts, provider updates, health check rounds, url-test and fallback
decisions, Δ warnings, max-failed events, API actions and runtime trends.
"""
import collections
import datetime
import gzip
import io
import json
import os
import re
import statistics
import sys
import tarfile

TOKEN = re.compile(r'([A-Za-z_][\w-]*)=("(?:[^"\\]|\\.)*"|\[[^\]]*\]|\S*)')


def unquote(v):
    if v.startswith('"'):
        try:
            return json.loads(v)
        except ValueError:
            return v[1:-1]
    return v


def parse(line):
    parts = line.rstrip("\n").split(" ", 2)
    if len(parts) < 2:
        return None
    try:
        t = datetime.datetime.fromisoformat(parts[0])
    except ValueError:
        return None
    rest = parts[2] if len(parts) > 2 else ""
    fields = {k: unquote(v) for k, v in TOKEN.findall(rest)}
    return t, parts[1], fields, rest


def read_sources(paths):
    for path in paths:
        if os.path.isdir(path):
            for name in sorted(os.listdir(path)):
                if name.startswith("diag-"):
                    yield from read_file(os.path.join(path, name))
        elif path.endswith((".tar.gz", ".tgz")):
            with tarfile.open(path) as tar:
                for m in sorted(tar.getmembers(), key=lambda m: m.name):
                    base = os.path.basename(m.name)
                    if m.isfile() and base.startswith("diag-"):
                        data = tar.extractfile(m).read()
                        if base.endswith(".gz"):
                            data = gzip.decompress(data)
                        yield from io.StringIO(data.decode("utf-8", "replace"))
        else:
            yield from read_file(path)


def read_file(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt", encoding="utf-8", errors="replace") as f:
        yield from f


def pct(values, p):
    if not values:
        return "-"
    values = sorted(values)
    return str(values[min(len(values) - 1, int(len(values) * p))])


def main(paths):
    events = [e for e in map(parse, read_sources(paths)) if e]
    events.sort(key=lambda e: e[0])
    if not events:
        print("no diagnostic lines found")
        return
    by = collections.defaultdict(list)
    for e in events:
        by[e[1]].append(e)
    first, last = events[0][0], events[-1][0]
    print(f"period: {first:%Y-%m-%d %H:%M:%S} … {last:%Y-%m-%d %H:%M:%S} ({last - first}), {len(events)} lines")
    for t, _, f, _ in by["start"]:
        print(f"  start {t:%Y-%m-%d %H:%M:%S} version={f.get('version')} go={f.get('go')} pid={f.get('pid')}")

    print("\n== providers")
    fetch = collections.Counter((f.get("name"), f.get("result")) for _, _, f, _ in by["fetch"])
    for (name, result), n in sorted(fetch.items()):
        print(f"  fetch {name}: {result} ×{n}")
    for t, _, _, rest in by["provider-update"]:
        print(f"  update {t:%m-%d %H:%M:%S} {rest[:240]}")

    print("\n== health check rounds (provider, reason: count, duration p50/p90/max ms, alive avg)")
    rounds = collections.defaultdict(list)
    for _, _, f, _ in by["hc-end"]:
        rounds[(f.get("provider"), f.get("reason"))].append((int(f.get("dur", 0)), int(f.get("alive", 0)), int(f.get("tests", 0))))
    for (prov, reason), rs in sorted(rounds.items()):
        durs = [r[0] for r in rs]
        print(f"  {prov} {reason}: {len(rs)} rounds, dur {pct(durs, .5)}/{pct(durs, .9)}/{max(durs)}, "
              f"alive {statistics.mean(r[1] for r in rs):.1f}/{statistics.mean(r[2] for r in rs):.1f}")
    skips = collections.Counter((f.get("provider"), f.get("reason")) for _, _, f, _ in by["hc-skip"])
    for (prov, reason), n in sorted(skips.items()):
        print(f"  skipped {prov} {reason}: ×{n}")
    for t, _, f, _ in by["hc-recheck"]:
        print(f"  recheck {t:%m-%d %H:%M:%S} {f.get('provider')}")

    print("\n== nodes (provider checks: ok%, delay p50, full p50/p90 ms)")
    nodes = collections.defaultdict(list)
    for _, _, f, _ in by["hc-node"]:
        nodes[(f.get("provider"), f.get("node"))].append(f)
    for (prov, node), fs in sorted(nodes.items(), key=lambda kv: (kv[0][0], -sum(x.get("ok") == "true" for x in kv[1]) / len(kv[1]))):
        ok = [x for x in fs if x.get("ok") == "true"]
        delays = [int(x["delay"]) for x in ok if x.get("delay", "-").isdigit()]
        fulls = [int(x["full"]) for x in ok if x.get("full", "").isdigit()]
        print(f"  {prov} {node}: {100 * len(ok) / len(fs):.0f}% of {len(fs)}, delay {pct(delays, .5)}, full {pct(fulls, .5)}/{pct(fulls, .9)}")

    print("\n== url-test decisions")
    switches = collections.defaultdict(collections.Counter)
    minute = collections.defaultdict(collections.Counter)
    for t, _, f, _ in by["url-test"]:
        g = f.get("group")
        switches[g][f.get("reason")] += 1
        if f.get("from") != f.get("to"):
            minute[g][t.minute] += 1
    for g, c in sorted(switches.items()):
        top = ", ".join(f"{m:02d}:×{n}" for m, n in minute[g].most_common(5))
        print(f"  {g}: {dict(c)}; switches by minute of hour: {top or '-'}")

    print("\n== url-test status (Δ = selected − best, ms)")
    status = collections.defaultdict(list)
    for t, _, f, _ in by["status"]:
        if "url-test" in f:
            status[f["url-test"]].append((t, f))
    for g, rows in sorted(status.items()):
        deltas = [int(f["delta"]) for _, f in rows if f.get("delta", "-").lstrip("-").isdigit()]
        over = [t for t, f in rows if f.get("delta", "-").lstrip("-").isdigit() and int(f["delta"]) > int(f.get("tol", 0))]
        fixed = {f.get("fixed") for _, f in rows if f.get("fixed")}
        alive = [int(f.get("alive", 0)) for _, f in rows]
        dials = sum(int(f.get("dial_ok", 0)) for _, f in rows), sum(int(f.get("dial_fail", 0)) for _, f in rows)
        hcs = sum(int(f.get("hc_ok", 0)) for _, f in rows), sum(int(f.get("hc_fail", 0)) for _, f in rows)  # since urltestfix5
        unreliable = [int(f["unreliable"]) for _, f in rows if f.get("unreliable", "").isdigit()]
        print(f"  {g}: {len(rows)} samples, Δ p50/p90/max {pct(deltas, .5)}/{pct(deltas, .9)}/{max(deltas) if deltas else '-'},"
              f" Δ>tol {len(over)}×, alive min/avg {min(alive)}/{statistics.mean(alive):.0f}, dials ok/fail {dials[0]}/{dials[1]},"
              f" fallback checks through it ok/fail {hcs[0]}/{hcs[1]}"
              + (f", unreliable alive avg {statistics.mean(unreliable):.0f}" if unreliable else "")
              + (f", FIXED {fixed}" if fixed else ""))

    print("\n== fallback")
    fb = collections.defaultdict(list)
    for t, _, f, _ in by["fallback"]:
        fb[f.get("group")].append((t, f.get("from"), f.get("to")))
    for g, sw in sorted(fb.items()):
        print(f"  {g}: {len(sw)} changes")
        for t, a, b in sw[:40]:
            print(f"    {t:%m-%d %H:%M:%S} {a!r} → {b!r}")

    print("\n== fallback re-checks (fail-confirm)")
    rc = collections.defaultdict(collections.Counter)
    for _, _, f, _ in by["fallback-confirm"]:
        rc[(f.get("group"), f.get("member"))][f.get("result")] += 1
    for (g, m), c in sorted(rc.items()):
        print(f"  {g} / {m}: alive ×{c['alive']}, dead ×{c['dead']}")
    if not rc:
        print("  none")

    print("\n== max-failed-times")
    mf = collections.Counter(f.get("group") for _, _, f, _ in by["max-failed"])
    for g, n in mf.most_common():
        print(f"  {g}: ×{n}")

    print("\n== API actions")
    for t, _, _, rest in by["api"][:60]:
        print(f"  {t:%m-%d %H:%M:%S} {rest[:180]}")
    if len(by["api"]) > 60:
        print(f"  … {len(by['api']) - 60} more")

    print("\n== warnings")
    w = collections.Counter(re.sub(r"\d+", "#", rest)[:120] for _, _, _, rest in by["warn"])
    for msg, n in w.most_common(15):
        print(f"  ×{n} {msg}")

    print("\n== runtime")
    rt = [(t, f) for t, _, f, _ in by["runtime"]]
    if rt:
        for key in ("goroutines", "heap", "alloc", "rss", "conns", "new_conns"):
            vals = [int(re.sub(r"\D", "", f.get(key, "0")) or 0) for _, f in rt if key in f]
            if not vals:  # alloc and new_conns: since urltestfix5
                continue
            print(f"  {key}: first {vals[0]}, min {min(vals)}, max {max(vals)}, last {vals[-1]}")
    for t, _, _, rest in by["error"]:
        print(f"  ERROR {t:%m-%d %H:%M:%S} {rest[:200]}")


if __name__ == "__main__":
    main(sys.argv[1:])
