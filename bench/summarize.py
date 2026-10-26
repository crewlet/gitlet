"""Summarize simulation runs: summarize.py WORK [ORACLE_JSON...] > RESULTS.md

WORK is the work directory of matrix.sh; runs are WORK/runs/*/result.json,
named PROFILE-sSEED-nAGENTS-MODE[-VARIANT]. ORACLE_JSON files are outputs of
gitlet-bench oracle, keyed by run name. Writes markdown tables to stdout and
every run as one CSV row to WORK/runs.csv.
"""

import collections
import csv
import glob
import json
import math
import os
import re
import statistics
import sys

NAME = re.compile(r"^(spread|clustered|hot)-s(\d+)-n(\d+)-(git|gitsync|filelock|gitlet)(?:-(\w+))?$")
MODES = ["git", "gitsync", "filelock", "gitlet"]
PROFILES = ["spread", "clustered", "hot"]


def wasted_lines(run_dir):
    """Lines of work thrown away: duplicates abandoned or found late."""
    n = 0
    p = os.path.join(run_dir, "events.jsonl")
    if not os.path.exists(p):
        return 0
    for line in open(p):
        e = json.loads(line)
        if e.get("kind") in ("abandon", "dup_late"):
            n += e.get("wastedLines", 0)
    return n


def load(work):
    runs = []
    for p in sorted(glob.glob(os.path.join(work, "runs", "*", "result.json"))):
        name = os.path.basename(os.path.dirname(p))
        m = NAME.match(name)
        if not m:
            continue
        r = json.load(open(p))
        t = r["totals"]
        out = r["outcomes"]
        landed = out.get("landed", 0)
        lost = sum(v for k, v in out.items() if k in ("stuck", "unfinished", "not_started", "abandoned_false_dup"))
        row = {
            "run": name,
            "profile": m.group(1),
            "seed": int(m.group(2)),
            "agents": int(m.group(3)),
            "mode": m.group(4),
            "variant": m.group(5) or "",
            "tasks": len(r["tasks"]),
            "rounds": r["rounds"],
            "landed": landed,
            "lost": lost,
            "dupsDropped": out.get("abandoned_dup", 0) + out.get("dup_late", 0) + out.get("empty", 0),
            "mainOK": r["mainVetOK"],
            "conflicts": t["conflicts"],
            "conflictLines": t["conflictLines"],
            "buildBreaks": t["buildBreaks"],
            "ciFailures": t["ciFailures"],
            "mergeConflicts": t["mergeConflicts"],
            "fixLines": t["fixLines"],
            "earlyFixLines": t["earlyFixLines"],
            "deferredLines": t["deferredLines"],
            "wastedLines": wasted_lines(os.path.dirname(p)),
            "workLines": t["workLines"],
            "waitRounds": t["waitRounds"],
            "busyRounds": t["busyRounds"],
            "blockedRounds": t["blockedRounds"],
            "coordCalls": t["coordCalls"],
            "submits": t["submits"],
            "wallSecs": round(r["wallSecs"], 1),
            "roundSecs": round(r["wallSecs"] / max(1, r["rounds"]), 2),
        }
        g = r.get("gitlet") or {}
        row["gitletCalls"] = sum((g.get("calls") or {}).values())
        row["gitletSecs"] = round(g.get("totalSecs", 0), 1)
        row["notes"] = g.get("notes", 0)
        row["noteChars"] = g.get("noteChars", 0)
        row["rework"] = row["conflictLines"] + row["fixLines"] + row["earlyFixLines"] + row["deferredLines"] + row["wastedLines"]
        runs.append(row)
    return runs


def mean(xs):
    return statistics.mean(xs) if xs else float("nan")


def fmt(x, d=1):
    if isinstance(x, float) and math.isnan(x):
        return "-"
    return f"{x:.{d}f}"


def table(header, rows):
    out = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    for r in rows:
        out.append("| " + " | ".join(str(c) for c in r) + " |")
    return "\n".join(out)


def main():
    work = sys.argv[1]
    runs = load(work)
    oracle = {}
    for p in sys.argv[2:]:
        oracle.update(json.load(open(p)))
    if runs:
        with open(os.path.join(work, "runs.csv"), "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=list(runs[0].keys()))
            w.writeheader()
            w.writerows(runs)
    main_runs = [r for r in runs if not r["variant"]]
    by = collections.defaultdict(list)
    for r in main_runs:
        by[(r["profile"], r["agents"], r["mode"])].append(r)

    print("## Makespan and rework, by profile, agents and mode\n")
    print("Means over seeds. Rounds is the makespan (a round is about a minute of agent work). Rework is lines of conflict, build fixes, adapting to stubs, work taken out to wait, and duplicate work thrown away.\n")
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            for m in MODES:
                rs = by.get((p, n, m), [])
                if not rs:
                    continue
                rows.append([p, n, m, len(rs), fmt(mean([r["rounds"] for r in rs])), fmt(mean([r["conflicts"] for r in rs])),
                             fmt(mean([r["conflictLines"] for r in rs])), fmt(mean([r["buildBreaks"] + r["ciFailures"] for r in rs])),
                             fmt(mean([r["rework"] for r in rs])), fmt(mean([r["waitRounds"] + r["blockedRounds"] for r in rs])),
                             fmt(mean([r["landed"] for r in rs])), sum(r["lost"] for r in rs), all(r["mainOK"] for r in rs)])
    print(table(["profile", "agents", "mode", "seeds", "rounds", "conflicts", "conflict lines", "build breaks + CI fails", "rework lines", "wait + blocked agent-rounds", "landed", "lost tasks", "main ok"], rows))

    print("\n## gitlet against plain git, paired by backlog and seed\n")
    print("Ratio of gitlet's makespan to git's and to gitsync's on the same backlog, and the change in conflicts and rework. Below 1 means gitlet was faster.\n")
    idx = {(r["profile"], r["seed"], r["agents"], r["mode"]): r for r in main_runs}
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            ratios, ratios_sync, dconf, drework = [], [], [], []
            for s in sorted({r["seed"] for r in main_runs}):
                g = idx.get((p, s, n, "gitlet"))
                b = idx.get((p, s, n, "git"))
                y = idx.get((p, s, n, "gitsync"))
                if g and b:
                    ratios.append(g["rounds"] / b["rounds"])
                    dconf.append(g["conflicts"] - b["conflicts"])
                    drework.append(g["rework"] - b["rework"])
                if g and y:
                    ratios_sync.append(g["rounds"] / y["rounds"])
            if ratios:
                rows.append([p, n, len(ratios), fmt(mean(ratios), 2), fmt(min(ratios), 2) + "-" + fmt(max(ratios), 2),
                             fmt(mean(ratios_sync), 2), fmt(mean(dconf)), fmt(mean(drework))])
    print(table(["profile", "agents", "pairs", "rounds gitlet/git", "range", "rounds gitlet/gitsync", "conflicts gitlet-git", "rework lines gitlet-git"], rows))

    print("\n## gitlet's overhead\n")
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            rs = by.get((p, n, "gitlet"), [])
            if not rs:
                continue
            tasks = sum(r["tasks"] for r in rs)
            rows.append([p, n, fmt(sum(r["gitletCalls"] for r in rs) / tasks), fmt(sum(r["coordCalls"] for r in rs) / tasks),
                         fmt(sum(r["notes"] for r in rs) / tasks), fmt(sum(r["noteChars"] for r in rs) / tasks, 0),
                         fmt(sum(r["gitletSecs"] for r in rs) / tasks), fmt(mean([r["roundSecs"] for r in rs]), 1)])
    print(table(["profile", "agents", "gitlet calls per task", "coordination calls per task", "notes read per task", "note characters per task", "seconds in gitlet per task", "wall seconds per round"], rows))

    variants = [r for r in runs if r["variant"]]
    if variants:
        print("\n## gitlet policy variants (8 agents)\n")
        print("defer is the main policy. ignore goes on despite overlap notes; half acts on each note with probability 0.5; actdup abandons duplicates only on gitlet's act-severity notes instead of judging every duplicate note rightly.\n")
        rows = []
        for p in PROFILES:
            for v in ["", "ignore", "half", "actdup"]:
                rs = [r for r in runs if r["profile"] == p and r["agents"] == 8 and r["mode"] == "gitlet" and r["variant"] == v]
                if not rs or (v == "" and not any(x["profile"] == p for x in variants)):
                    continue
                gits = [idx.get((p, r["seed"], 8, "git")) for r in rs]
                ratio = [r["rounds"] / b["rounds"] for r, b in zip(rs, gits) if b]
                rows.append([p, v or "defer", len(rs), fmt(mean([r["rounds"] for r in rs])), fmt(mean(ratio), 2), fmt(mean([r["conflicts"] for r in rs])),
                             fmt(mean([r["rework"] for r in rs])), fmt(mean([r["waitRounds"] + r["blockedRounds"] for r in rs])), sum(r["lost"] for r in rs)])
        print(table(["profile", "policy", "seeds", "rounds", "rounds vs git", "conflicts", "rework lines", "wait + blocked", "lost tasks"], rows))

    if oracle:
        print("\n## Did gitlet's notes name the pairs that really interact?\n")
        print("Pairs of tasks in flight at the same time. Interacting pairs conflict textually, break the build together, or duplicate each other when both are made on the base. Recall: interacting pairs named by a warn or act note. Precision: warn or act pairs that interact.\n")
        rows = []
        agg = collections.defaultdict(lambda: collections.Counter())
        for name, d in oracle.items():
            m = NAME.match(name)
            if not m or m.group(5):
                continue
            agg[(m.group(1), int(m.group(3)))].update(d)
        for (p, n), d in sorted(agg.items(), key=lambda x: (PROFILES.index(x[0][0]), x[0][1])):
            rec = d["hitWarnPairs"] / d["interactingPairs"] if d["interactingPairs"] else float("nan")
            prec = d["hitWarnPairs"] / d["flaggedWarnPairs"] if d["flaggedWarnPairs"] else float("nan")
            share = d["notesOnTroublePairs"] / (d["notesOnTroublePairs"] + d["notesOnQuietPairs"]) if d["notesOnTroublePairs"] + d["notesOnQuietPairs"] else float("nan")
            lead = d["leadRoundsSum"] / d["leadRoundsHits"] if d["leadRoundsHits"] else float("nan")
            rows.append([p, n, d["concurrentPairs"], d["interactingPairs"], f'{d["conflictPairs"]}/{d["breakPairs"]}/{d["dupPairs"]}', d["flaggedWarnPairs"],
                         fmt(rec * 100, 0) + "%", fmt(prec * 100, 0) + "%", f'{d["hitDupPairs"]}/{d["dupPairs"]}', fmt(share * 100, 0) + "%", fmt(lead)])
        print(table(["profile", "agents", "concurrent pairs", "interacting", "conflict/break/dup", "warned pairs", "recall", "precision", "dups named", "notes about interacting pairs", "lead rounds"], rows))


if __name__ == "__main__":
    main()
