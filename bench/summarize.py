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


def defer_waits(run_dir):
    """Op-rounds waited, by what deferred the op: who, or the note kind."""
    waits = collections.Counter()
    p = os.path.join(run_dir, "events.jsonl")
    if not os.path.exists(p):
        return waits
    why = {}
    for line in open(p):
        e = json.loads(line)
        if e.get("kind") == "defer":
            for op in e.get("ops") or []:
                why[(e["task"], op)] = e.get("noteKind", "")
        elif e.get("kind") == "undefer":
            waits[why.get((e["task"], e["op"]), "")] += e.get("waited", 0)
    return waits


def landed_twice(tasks):
    """Duplicate tasks that landed next to their original, and their lines."""
    out = {t["id"]: t.get("outcome") for t in tasks}
    n = lines = 0
    for t in tasks:
        if t.get("dupOf") and t.get("outcome") == "landed" and out.get(t["dupOf"]) == "landed":
            n += 1
            lines += t.get("workLines", 0)
    return n, lines


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
        row["dupsTwice"], row["twiceLines"] = landed_twice(r["tasks"])
        waits = defer_waits(os.path.dirname(p))
        row["waitWho"] = waits["who"]
        row["waitPredicted"] = waits["predicted_conflict"]
        row["waitBodyOverlap"] = waits["body_overlap"]
        g = r.get("gitlet") or {}
        row["gitletCalls"] = sum((g.get("calls") or {}).values())
        row["gitletSecs"] = round(g.get("totalSecs", 0), 1)
        row["notes"] = g.get("notes", 0)
        row["noteChars"] = g.get("noteChars", 0)
        row["rework"] = row["conflictLines"] + row["fixLines"] + row["earlyFixLines"] + row["deferredLines"] + row["wastedLines"] + row["twiceLines"]
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
    print("Means over seeds. Rounds is the makespan (a round is about a minute of agent work). Rework is lines of conflict, build fixes, adapting to stubs or main, work taken out to wait, duplicate work thrown away, and duplicates that landed next to their original.\n")
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

    print("\n## Where the rework comes from\n")
    print("Mean lines per run. Conflict: lines of conflicts resolved by hand. Build fix: lines changed fixing a build broken by a clean rebase or sent back by CI. Stub: lines changed adapting to a gitlet stub, or to main after a stale or new_callers note. Deferred: lines taken out to wait for another task. Duplicate: work thrown away because another task did the same. Landed twice: lines of duplicates that landed next to their original. The last two columns count the duplicate tasks that ended each way.\n")
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            for m in MODES:
                rs = by.get((p, n, m), [])
                if not rs:
                    continue
                rows.append([p, n, m, len(rs)] + [fmt(mean([r[k] for r in rs])) for k in ("conflictLines", "fixLines", "earlyFixLines", "deferredLines", "wastedLines", "twiceLines", "dupsDropped", "dupsTwice")])
    print(table(["profile", "agents", "mode", "seeds", "conflict", "build fix", "stub", "deferred", "duplicate", "landed twice", "duplicates dropped", "duplicates landed twice"], rows))

    print("\n## Where the time goes\n")
    print("Mean agent-rounds per run. Busy: resolving conflicts, fixing builds, adapting, and (gitlet) coordination calls at a quarter round each. Wait: nothing runnable because ops were deferred or files locked. Blocked: a finished task waiting for a contract to land.\n")
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            for m in MODES:
                rs = by.get((p, n, m), [])
                if not rs:
                    continue
                rows.append([p, n, m, len(rs)] + [fmt(mean([r[k] for r in rs])) for k in ("busyRounds", "waitRounds", "blockedRounds")])
    print(table(["profile", "agents", "mode", "seeds", "busy", "wait", "blocked"], rows))

    print("\n## gitlet against plain git, paired by backlog and seed\n")
    print("Ratio of gitlet's makespan to git's and to gitsync's on the same backlog, and the change in conflicts and rework. Below 1 means gitlet was faster.\n")
    idx = {(r["profile"], r["seed"], r["agents"], r["mode"]): r for r in main_runs}
    rows = []
    for p in PROFILES:
        for n in sorted({r["agents"] for r in main_runs}):
            ratios, ratios_sync, dconf, drework, sign = [], [], [], [], collections.Counter()
            for s in sorted({r["seed"] for r in main_runs}):
                g = idx.get((p, s, n, "gitlet"))
                b = idx.get((p, s, n, "git"))
                y = idx.get((p, s, n, "gitsync"))
                if g and b:
                    ratios.append(g["rounds"] / b["rounds"])
                    sign["faster" if g["rounds"] < b["rounds"] else "slower" if g["rounds"] > b["rounds"] else "equal"] += 1
                    dconf.append(g["conflicts"] - b["conflicts"])
                    drework.append(g["rework"] - b["rework"])
                if g and y:
                    ratios_sync.append(g["rounds"] / y["rounds"])
            if ratios:
                rows.append([p, n, len(ratios), fmt(mean(ratios), 2), fmt(min(ratios), 2) + "-" + fmt(max(ratios), 2),
                             f'{sign["faster"]}/{sign["equal"]}/{sign["slower"]}', fmt(mean(ratios_sync), 2), fmt(mean(dconf)), fmt(mean(drework))])
    print(table(["profile", "agents", "pairs", "rounds gitlet/git", "range", "faster/equal/slower", "rounds gitlet/gitsync", "conflicts gitlet-git", "rework lines gitlet-git"], rows))

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
        print("\n## gitlet policy variants\n")
        print("defer is the main policy: an op waits for an earlier task that a warn or act body_overlap or predicted_conflict note names, or that `who` lists as changing the function. notes waits on the notes but not on `who`. ignore never waits. half acts on each warn or act note with probability 0.5 (the `who` wait is unchanged). actdup abandons duplicates only on act-severity notes instead of judging every info duplicate note rightly. The last three columns split the op-rounds waited by what deferred the op.\n")
        rows = []
        for n in sorted({r["agents"] for r in variants}):
            for p in PROFILES:
                if not any(x["profile"] == p and x["agents"] == n for x in variants):
                    continue
                for v in ["", "notes", "ignore", "half", "actdup"]:
                    rs = [r for r in runs if r["profile"] == p and r["agents"] == n and r["mode"] == "gitlet" and r["variant"] == v]
                    if not rs:
                        continue
                    gits = [idx.get((p, r["seed"], n, "git")) for r in rs]
                    ratio = [r["rounds"] / b["rounds"] for r, b in zip(rs, gits) if b]
                    rows.append([p, n, v or "defer", len(rs), fmt(mean([r["rounds"] for r in rs])), fmt(mean(ratio), 2), fmt(min(ratio), 2) + "-" + fmt(max(ratio), 2),
                                 fmt(mean([r["conflicts"] for r in rs])), fmt(mean([r["buildBreaks"] + r["ciFailures"] for r in rs])),
                                 fmt(mean([r["rework"] for r in rs])), fmt(mean([r["waitRounds"] + r["blockedRounds"] for r in rs])), sum(r["dupsTwice"] for r in rs), sum(r["lost"] for r in rs),
                                 fmt(mean([r["waitWho"] for r in rs])), fmt(mean([r["waitPredicted"] for r in rs])), fmt(mean([r["waitBodyOverlap"] for r in rs]))])
        print(table(["profile", "agents", "policy", "seeds", "rounds", "rounds vs git", "range", "conflicts", "build breaks + CI fails", "rework lines", "wait + blocked", "duplicates landed twice", "lost tasks",
                     "waited on who", "on predicted_conflict", "on body_overlap"], rows))

    if oracle:
        print("\n## Did gitlet's notes name the pairs that really interact?\n")
        print("Pairs of tasks in flight at the same time. Interacting pairs conflict textually, break the build together, or duplicate each other when both are made on the base. Recall: interacting pairs named by a warn or act note; without duplicates, the same for pairs that are not duplicates (duplicate notes are info, so they never count, and most duplicate pairs also conflict). Precision: warn or act pairs that interact. Notes about interacting pairs: the share of all notes (info included) that name an interacting pair. Lead rounds: from the first warn or act note on an interacting pair to the first of the two landing. Duplicates named: true duplicate pairs a duplicate note named, of all true duplicate pairs in flight together.\n")
        rows = []
        agg = collections.defaultdict(lambda: collections.Counter())
        for name, d in oracle.items():
            m = NAME.match(name)
            if not m or m.group(5):
                continue
            agg[(m.group(1), int(m.group(3)))].update(d)
        for (p, n), d in sorted(agg.items(), key=lambda x: (PROFILES.index(x[0][0]), x[0][1])):
            rec = d["hitWarnPairs"] / d["interactingPairs"] if d["interactingPairs"] else float("nan")
            rec_hard = d["hitWarnNoDupPairs"] / d["interactingNoDupPairs"] if d["interactingNoDupPairs"] else float("nan")
            prec = d["hitWarnPairs"] / d["flaggedWarnPairs"] if d["flaggedWarnPairs"] else float("nan")
            share = d["notesOnTroublePairs"] / (d["notesOnTroublePairs"] + d["notesOnQuietPairs"]) if d["notesOnTroublePairs"] + d["notesOnQuietPairs"] else float("nan")
            lead = d["leadRoundsSum"] / d["leadRoundsHits"] if d["leadRoundsHits"] else float("nan")
            rows.append([p, n, d["concurrentPairs"], d["interactingPairs"], f'{d["conflictPairs"]}/{d["breakPairs"]}/{d["dupPairs"]}', d["flaggedWarnPairs"], d["hitWarnPairs"],
                         fmt(rec * 100, 0) + "%", fmt(rec_hard * 100, 0) + "%", fmt(prec * 100, 0) + "%", fmt(share * 100, 0) + "%", fmt(lead),
                         f'{d["hitDupPairs"]}/{d["dupPairs"]}', d["flaggedDupPairs"], f'{d["hitDupActPairs"]}/{d["flaggedDupActPairs"]}'])
        print(table(["profile", "agents", "concurrent pairs", "interacting", "conflict/break/dup", "warned pairs", "warned and interacting", "recall", "recall without duplicates", "precision", "notes about interacting pairs", "lead rounds",
                     "duplicates named", "pairs named by duplicate notes", "act duplicate notes right/all"], rows))


if __name__ == "__main__":
    main()
