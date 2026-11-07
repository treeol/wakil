#!/usr/bin/env python3
"""Summarise the timeline files written by run.sh.

  summarize.py PERF_DIR REPO_DIR...

For each repo: per transition the cold run (run 1) and the median/min/max of
the warm runs (2..N) of TIME TO READY, and the five costliest phases (leaf
phases, by median over warm runs). "Time to ready" is when the user can type
again: for "start" the offset of the "prompt ready" instant, for /new and
/resume the offset of "rotation applied in tui", for "exit" the transition's
total. The lazy sandbox session start is reported as its own line because it
falls in whatever transition first reaches the sandbox."""
import glob, json, os, statistics, sys

def load(path):
    with open(path) as f:
        return json.load(f)

def ready_ms(t):
    ends = [s["start_ms"] for s in t["spans"] if s.get("instant") and ("prompt ready" in s["name"] or "rotation applied" in s["name"])]
    return min(ends) if ends else t["total_ms"]

def leaves(t):
    out = []
    sp = [s for s in t["spans"] if not s.get("instant")]
    for i, s in enumerate(sp):
        parent = False
        for j, o in enumerate(sp):
            if i == j or o.get("accumulated"):
                continue
            if o.get("depth", 0) > s.get("depth", 0) and o["start_ms"] >= s["start_ms"] and o["start_ms"] + o["dur_ms"] <= s["start_ms"] + s["dur_ms"] + 0.01:
                parent = True
                break
        if not parent:
            out.append(s)
    return out

def covered(t, upto):
    """Milliseconds of [0, upto] covered by at least one wall-clock span
    (concurrent and accumulated spans overlap other work or are sums, so they
    do not count as coverage)."""
    iv = sorted((max(0.0, x["start_ms"]), min(upto, x["start_ms"] + x["dur_ms"]))
                for x in t["spans"]
                if not x.get("instant") and not x.get("concurrent") and not x.get("accumulated"))
    total, cur_end = 0.0, 0.0
    for a, b in iv:
        if b <= a:
            continue
        a = max(a, cur_end)
        if b > a:
            total += b - a
            cur_end = b
    return total

def med(xs):
    return statistics.median(xs) if xs else float("nan")

def fmt(x):
    return "%7.1f" % x if x == x else "    n/a"

def main():
    perf = sys.argv[1]
    for repo in sys.argv[2:]:
        name = os.path.basename(os.path.abspath(repo))
        files = sorted(glob.glob(os.path.join(perf, name, "data", "wakil", "traces", "timeline-*.json")), key=os.path.getmtime)
        if not files:
            print("### %s\n\nno timeline files found under %s\n" % (name, perf))
            continue
        docs = [load(f) for f in files]
        host = docs[0].get("host", "?")
        print("### %s  (%d runs, host %s, wakil %s)\n" % (name, len(docs), host, docs[0].get("version", "?")))
        names = []
        for d in docs:
            for t in d["transitions"]:
                if t["name"] not in names:
                    names.append(t["name"])
        print("| transition | cold (run 1) | warm median | warm min | warm max | unattributed (warm median) | samples |")
        print("|---|---:|---:|---:|---:|---:|---:|")
        per = {}
        for n in names:
            vals = []
            gaps = []
            for k, d in enumerate(docs):
                # /new occurs twice per run: take the first as the typical one
                ts = [t for t in d["transitions"] if t["name"] == n]
                if ts:
                    r = ready_ms(ts[0])
                    vals.append((k, r))
                    if k > 0 or len(docs) == 1:
                        gaps.append(max(0.0, r - covered(ts[0], r)))
            per[n] = vals
            cold = next((v for k, v in vals if k == 0), float("nan"))
            warm = [v for k, v in vals if k > 0]
            print("| %s | %s ms | %s ms | %s ms | %s ms | %s ms | %d |" % (n, fmt(cold), fmt(med(warm)), fmt(min(warm) if warm else float("nan")), fmt(max(warm) if warm else float("nan")), fmt(med(gaps)), len(vals)))
        print()
        for n in names:
            by = {}
            for k, d in enumerate(docs):
                if k == 0 and len(docs) > 1:
                    continue  # warm only
                for t in d["transitions"]:
                    if t["name"] != n:
                        continue
                    for s in leaves(t):
                        by.setdefault(s["name"], []).append(s["dur_ms"])
                    break
            top = sorted(((med(v), k) for k, v in by.items()), reverse=True)[:5]
            if not top:
                continue
            print("top five phases of `%s` (warm median):\n" % n)
            for m, k in top:
                print("- %s ms  %s" % (fmt(m).strip(), k))
            print()

main()
