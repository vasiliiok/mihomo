#!/usr/bin/env python3
"""Integration scenarios S1–S6 for mihomo url-test / health-check fixes.

usage: scenarios.py <mihomo binary> <build label> <out dir> [scenario ...]

Every scenario starts a fresh sim.py (SOCKS5 nodes + HTTP 204 target) and a
fresh mihomo with a file provider, then changes the provider file or the node
behaviour and watches the groups through the external controller.
"""
import datetime
import json
import os
import re
import shutil
import subprocess
import sys
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
TARGET, CONTROL, MIXED, API = 18204, 18300, 17890, 19090
URL = f"http://127.0.0.1:{TARGET}/generate_204"
URL2 = f"http://127.0.0.1:{TARGET}/other_204"
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # ignore *_PROXY


def http(method, url, body=None, timeout=15):
    req = urllib.request.Request(url, data=body, method=method)
    with OPENER.open(req, timeout=timeout) as resp:
        return resp.read()


def api(path, timeout=15):
    return json.loads(http("GET", f"http://127.0.0.1:{API}{path}", timeout=timeout) or b"{}")


def sim_set(cfg):
    http("POST", f"http://127.0.0.1:{CONTROL}/ports", json.dumps(cfg).encode())


def sim_stats():
    return json.loads(http("GET", f"http://127.0.0.1:{CONTROL}/stats"))


def clean_env():
    env = {k: v for k, v in os.environ.items() if "proxy" not in k.lower()}
    return env


class Run:
    def __init__(self, binary, build, scenario, out_dir):
        self.binary, self.build, self.scenario = binary, build, scenario
        self.dir = os.path.join(out_dir, f"{scenario}-{build}")
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        self.events = []
        self.t0 = time.time()
        self.sim = self.mihomo = None
        self.result = {}

    # ---- helpers -------------------------------------------------------
    def log(self, msg):
        line = f"[{time.time() - self.t0:7.2f}s] {msg}"
        self.events.append(line)
        print(f"  {self.scenario}/{self.build} {line}", flush=True)

    def write_nodes(self, nodes, comment=None):
        lines = []
        if comment:
            lines.append(f"# {comment}")
        lines.append("proxies:")
        for name, port in nodes:
            lines.append(f"  - {{name: {name}, type: socks5, server: 127.0.0.1, port: {port}, udp: false}}")
        tmp = os.path.join(self.dir, "sim.yaml.tmp")
        with open(tmp, "w") as f:
            f.write("\n".join(lines) + "\n")
        os.replace(tmp, os.path.join(self.dir, "sim.yaml"))

    def start(self, ports, nodes, groups, interval, timeout, rules="MATCH,FB", extra_hc=""):
        self.sim = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "sim.py"), str(TARGET), str(CONTROL),
                                     json.dumps({str(p): c for p, c in ports.items()})],
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        assert self.sim.stdout.readline().strip() == "sim ready"
        self.write_nodes(nodes)
        config = f"""mixed-port: {MIXED}
external-controller: 127.0.0.1:{API}
log-level: debug
unified-delay: true
ipv6: false
mode: rule
profile: {{store-selected: false, store-fake-ip: false}}
proxy-providers:
  sim:
    type: file
    path: ./sim.yaml
    health-check: {{enable: true, url: "{URL}", expected-status: 204, interval: {interval}, timeout: {timeout}, lazy: false{extra_hc}}}
proxy-groups:
{groups}
rules:
  - {rules}
"""
        with open(os.path.join(self.dir, "config.yaml"), "w") as f:
            f.write(config)
        self.logfile = open(os.path.join(self.dir, "mihomo.log"), "w")
        self.t0 = time.time()
        self.mihomo = subprocess.Popen([self.binary, "-d", self.dir], stdout=self.logfile, stderr=subprocess.STDOUT,
                                       env=clean_env())
        for _ in range(100):
            try:
                api("/version")
                break
            except Exception:
                time.sleep(0.05)
        self.log(f"mihomo started: {api('/version')}")

    def stop(self):
        for p in (self.mihomo, self.sim):
            if p is not None:
                p.terminate()
                try:
                    p.wait(5)
                except subprocess.TimeoutExpired:
                    p.kill()
        if self.mihomo is not None:
            self.logfile.close()
        with open(os.path.join(self.dir, "events.txt"), "w") as f:
            f.write("\n".join(self.events) + "\n")
            f.write("RESULT " + json.dumps(self.result, ensure_ascii=False) + "\n")

    def now(self, group):
        return api(f"/proxies/{group}")["now"]

    def wait_now(self, group, want, timeout):
        end = time.time() + timeout
        while time.time() < end:
            try:
                if self.now(group) == want:
                    return True
            except Exception:
                pass
            time.sleep(0.2)
        return False

    def watch(self, groups, duration, step=0.15, traffic=False):
        """poll `now` of groups, return the list of (t, group, now) changes"""
        changes, last, end = [], {}, time.time() + duration
        next_traffic = time.time()
        while time.time() < end:
            if traffic and time.time() >= next_traffic:
                threading.Thread(target=self.curl, daemon=True).start()
                next_traffic += 1
            for g in groups:
                try:
                    n = self.now(g)
                except Exception as e:
                    n = f"<{type(e).__name__}>"
                if last.get(g) != n:
                    last[g] = n
                    changes.append((round(time.time() - self.t0, 2), g, n))
                    self.log(f"{g}.now = {n}")
            time.sleep(step)
        return changes

    def curl(self):
        r = subprocess.run(["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "3",
                            "-x", f"http://127.0.0.1:{MIXED}", URL], capture_output=True, text=True, env=clean_env())
        return r.stdout

    def tested(self, url=URL):
        """names of current provider proxies that have a result for url"""
        pd = api("/providers/proxies/sim")
        return [p["name"] for p in pd["proxies"] if p.get("extra", {}).get(url, {}).get("history")]

    def log_events(self, pattern, since=0.0):
        """(t, line) of mihomo log lines matching pattern, t relative to t0"""
        out = []
        with open(os.path.join(self.dir, "mihomo.log")) as f:
            for line in f:
                if not re.search(pattern, line):
                    continue
                m = re.search(r'time="([^"]+)"', line)
                if not m:
                    continue
                ts = re.sub(r"(\.\d{6})\d*", r"\1", m.group(1)).replace("Z", "+00:00")
                t = datetime.datetime.fromisoformat(ts).timestamp() - self.t0
                if t >= since:
                    out.append((round(t, 2), re.search(r'msg="(.*)"', line).group(1)))
        return out


def groups_basic(tolerance=50, fb_interval=10):
    return f"""  - {{name: UT, type: url-test, url: "{URL}", tolerance: {tolerance}, max-failed-times: 3, include-all: true, exclude-filter: '^b', lazy: false}}
  - {{name: BK, type: url-test, url: "{URL}", tolerance: {tolerance}, max-failed-times: 3, include-all: true, filter: '^b', lazy: false}}
  - {{name: FB, type: fallback, proxies: [UT, BK], url: "{URL}", expected-status: 204, interval: {fb_interval}, max-failed-times: 3, lazy: false}}"""


def ok(latency, connect_delay=0):
    return {"latency": latency, "connect_delay": connect_delay, "mode": "ok"}


# ---- S1: the selected node is recreated with a different port ---------------
def s1(run):
    ports = {20001: ok(200), 20002: ok(150), 20003: ok(40), 20004: ok(180), 20101: ok(100),
             21003: ok(400)}  # the new port of n3: same node, much slower
    nodes = [("n1", 20001), ("n2", 20002), ("n3", 20003), ("n4", 20004), ("b1", 20101)]
    run.start(ports, nodes, groups_basic(), interval=15, timeout=2000)
    assert run.wait_now("UT", "n3", 30), "UT did not settle on n3"
    run.log("UT settled on n3 (40 ms); moving n3 to port 21003 (400 ms), old port 20003 keeps working")
    before = sim_stats()
    nodes[2] = ("n3", 21003)
    run.write_nodes(nodes, "n3 moved")
    t_upd = time.time() - run.t0
    # the first 12 s cover the 10 s fastSingle cache of url-test
    changes = run.watch(["UT"], 12, traffic=True)
    mid = sim_stats()
    changes += run.watch(["UT"], 33, traffic=True)
    after = sim_stats()
    early = mid.get("20003", 0) - before.get("20003", 0)
    late = after.get("20003", 0) - mid.get("20003", 0)
    final = run.now("UT")
    run.result = {"final_UT": final, "old_port_conns_0_12s": early, "old_port_conns_12_45s": late,
                  "UT_changes": changes, "update_at": round(t_upd, 2)}
    run.result["fixed"] = final != "n3" and late == 0


# ---- S2: all nodes recreated without changes, selected node is not first ----
def s2(run):
    # 30 dead nodes (accept TCP, never answer) come first: with 10 tests at a time
    # and timeout 5 s nothing alive is tested during the first 15 s of a check,
    # longer than the 10 s url-test cache, so a pick inside that window is certain.
    ports, nodes = {}, []
    for i in range(1, 31):
        ports[20000 + i] = {"mode": "hang", "latency": 0, "connect_delay": 0}
        nodes.append((f"h{i:02d}", 20000 + i))
    for name, port, latency in (("m1", 20031, 250), ("sel", 20032, 120), ("m2", 20033, 300)):
        ports[port] = ok(latency)
        nodes.append((name, port))
    ports[20101] = ok(100)
    nodes.append(("b1", 20101))
    run.start(ports, nodes, groups_basic(), interval=90, timeout=5000)
    assert run.wait_now("UT", "sel", 50), "UT did not settle on sel"
    run.wait_now("FB", "UT", 20)
    time.sleep(1)
    run.log("UT on sel (120 ms), FB on UT; recreating all nodes (comment in the file)")
    run.write_nodes(nodes, "touch")
    t_upd = time.time() - run.t0
    changes = run.watch(["UT", "FB"], 40, traffic=True)
    checked = run.log_events(r"Health Checked, proxy: (sel|m1|h01),", since=t_upd)
    run.result = {"update_at": round(t_upd, 2), "changes": changes,
                  "checks_after_update": checked[:6]}
    ut = [n for (_, g, n) in changes if g == "UT"]
    fb = [n for (_, g, n) in changes if g == "FB"]
    run.result["fixed"] = set(ut) == {"sel"} and set(fb) <= {"UT"}


# ---- S3: tolerance when the selected node is the first one -------------------
def s3(run):
    ports = {20001: ok(60), 20002: ok(150), 20003: ok(200), 20101: ok(100)}
    nodes = [("n1", 20001), ("n2", 20002), ("n3", 20003), ("b1", 20101)]
    run.start(ports, nodes, groups_basic(), interval=10, timeout=2000)
    end = time.time() + 20
    while time.time() < end and len(run.tested()) < len(nodes):
        time.sleep(0.2)
    time.sleep(10.5)  # let the url-test cache expire, so the pick below is made on test results
    assert run.wait_now("UT", "n1", 15), "UT did not settle on n1"
    run.log("UT on n1 (first in list); n1 -> 110 ms, n2 -> 80 ms (better by 30 < tolerance 50)")
    sim_set({"20001": {"latency": 110}, "20002": {"latency": 80}})
    phase1 = run.watch(["UT"], 32)
    stayed = run.now("UT")
    run.log("n2 -> 40 ms (better by 70 > tolerance 50): every build must switch")
    sim_set({"20002": {"latency": 40}})
    phase2 = run.watch(["UT"], 32)
    switched = run.now("UT")
    run.result = {"within_tolerance_final": stayed, "beyond_tolerance_final": switched,
                  "changes": phase1 + phase2}
    run.result["fixed"] = stayed == "n1" and switched == "n2"


# ---- S4: subscription update during a running health check -------------------
def s4(run):
    ports, nodes = {}, []
    for i in range(1, 51):
        ports[20000 + i] = ok(50 + i, connect_delay=1500)
        nodes.append((f"n{i:02d}", 20000 + i))
    ports[20101] = ok(100)
    nodes.append(("b1", 20101))
    run.start(ports, nodes, groups_basic(), interval=120, timeout=5000)
    end = time.time() + 40
    while time.time() < end and len(run.tested()) < 51:
        time.sleep(0.5)
    run.log(f"startup check done: {len(run.tested())}/51 tested")
    time.sleep(2)
    th = threading.Thread(target=lambda: api("/providers/proxies/sim/healthcheck", timeout=60), daemon=True)
    th.start()
    run.log("manual health check started (takes ~8 s: 51 nodes, 10 at a time, 1.5 s handshake)")
    time.sleep(2.5)
    run.write_nodes(nodes, "update during check")
    t_write = time.time() - run.t0
    upd = []
    while not upd and time.time() - run.t0 < t_write + 10:  # wait for fswatch to reload
        time.sleep(0.05)
        upd = run.log_events(r"content update", since=t_write - 1)
    t_upd = upd[0][0]
    run.log(f"provider reloaded in the middle of the check (log time {t_upd:.2f}s)")
    first_any = first_all = None
    timeline = []
    end = time.time() + 30
    while time.time() < end:
        n = len(run.tested())
        t = round(time.time() - run.t0 - t_upd, 1)
        if not timeline or timeline[-1][1] != n:
            timeline.append((t, n))
        if n and first_any is None:
            first_any = t
        if n == 51 and first_all is None:
            first_all = t
        time.sleep(0.5)
    starts = run.log_events(r"Start New Health Checking", since=0)
    run.result = {"update_at": round(t_upd, 2), "new_objects_tested_timeline(s_after_update, count)": timeline,
                  "first_new_tested_after_s": first_any, "all_new_tested_after_s": first_all,
                  "health_check_starts": [t for t, _ in starts]}
    run.result["fixed"] = first_all is not None and first_all < 15


# ---- S5: regression: startup, node death, max-failed-times, fallback ---------
def s5(run):
    ports = {20001: ok(150), 20002: ok(60), 20003: ok(100), 20101: ok(80), 20102: ok(120)}
    nodes = [("n1", 20001), ("n2", 20002), ("n3", 20003), ("b1", 20101), ("b2", 20102)]
    run.start(ports, nodes, groups_basic(), interval=30, timeout=2000)
    startup = run.watch(["UT", "BK", "FB"], 12, step=0.1)
    res = {"startup": startup}
    assert run.wait_now("UT", "n2", 30)
    # A: the selected node dies, scheduled check finds it
    sim_set({"20002": {"mode": "hang"}})
    t = time.time()
    run.log("A: n2 (selected) now hangs; waiting for a scheduled check")
    run.wait_now("UT", "n3", 50)
    res["A_left_dead_node_after_s"] = round(time.time() - t, 1)
    res["A_UT"] = run.now("UT")
    # B: the selected node fails dials: max-failed-times
    run.wait_now("FB", "UT", 30)  # traffic must reach UT
    sim_set({"20003": {"mode": "close"}})
    run.log("B: n3 (selected) now closes connections; sending traffic")
    t = time.time()
    for _ in range(6):
        threading.Thread(target=run.curl, daemon=True).start()
        time.sleep(0.3)
    run.wait_now("UT", "n1", 25)
    res["B_left_failing_node_after_s"] = round(time.time() - t, 1)
    res["B_UT"] = run.now("UT")
    hc = run.log_events(r"failed multiple times, activate health check")
    res["B_max_failed_triggered"] = [x[1] for x in hc]
    # C: everything in UT dies -> fallback to BK, then back
    sim_set({"20001": {"mode": "hang"}, "20002": {"mode": "hang"}, "20003": {"mode": "hang"}})
    run.log("C: all UT nodes hang")
    t = time.time()
    run.wait_now("FB", "BK", 60)
    res["C_FB_to_BK_after_s"] = round(time.time() - t, 1)
    sim_set({"20001": {"mode": "ok"}, "20002": {"mode": "ok"}, "20003": {"mode": "ok"}})
    run.log("C: UT nodes revived")
    t = time.time()
    run.wait_now("FB", "UT", 60)
    res["C_FB_back_to_UT_after_s"] = round(time.time() - t, 1)
    res["C_UT_after_revival"] = run.now("UT")
    res["fixed"] = res["A_UT"] == "n3" and res["B_UT"] == "n1" and run.now("FB") == "UT"
    run.result = res


# ---- S6: a selected node that is never tested with the group url -------------
def s6(run):
    # The provider tests URL for every node. Groups JP and ALL both use URL2;
    # JP registers it with filter '^j', ALL without a filter, so URL2 is tested
    # only for j-nodes (an upstream quirk). ALL starts on x1 before any test.
    # slow handshakes keep the first url2 results ~1.5 s away, so ALL picks before them
    ports = {20001: ok(100), 20002: ok(50, 1500), 20003: ok(80, 1500), 20004: ok(150)}
    nodes = [("x1", 20001), ("j1", 20002), ("j2", 20003), ("x2", 20004)]
    groups = f"""  - {{name: JP, type: url-test, url: "{URL2}", tolerance: 30, include-all: true, filter: '^j', lazy: false}}
  - {{name: ALL, type: url-test, url: "{URL2}", tolerance: 30, include-all: true, lazy: false}}"""
    run.start(ports, nodes, groups, interval=10, timeout=2000, rules="MATCH,ALL")
    changes = run.watch(["ALL", "JP"], 35, step=0.1)
    final = run.now("ALL")
    run.result = {"changes": changes, "final_ALL": final,
                  "x1_tested_with_group_url": "x1" in run.tested(URL2),
                  "j1_tested_with_group_url": "j1" in run.tested(URL2)}
    run.result["fixed"] = final == "j1"


SCENARIOS = {"S1": s1, "S2": s2, "S3": s3, "S4": s4, "S5": s5, "S6": s6}

if __name__ == "__main__":
    binary, build, out_dir = sys.argv[1:4]
    names = sys.argv[4:] or list(SCENARIOS)
    summary = {}
    for name in names:
        run = Run(binary, build, name, out_dir)
        try:
            SCENARIOS[name](run)
        except Exception as e:  # keep going, record the failure
            run.result["error"] = repr(e)
            run.log(f"ERROR {e!r}")
        finally:
            run.stop()
        summary[name] = run.result
        time.sleep(1)
    path = os.path.join(out_dir, f"summary-{build}.json")
    if os.path.exists(path):  # a rerun of some scenarios replaces only their entries
        with open(path) as f:
            summary = {**json.load(f), **summary}
    with open(path, "w") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
