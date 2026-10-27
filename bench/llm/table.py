"""Tabulate the agent study: table.py RUNS.jsonl... > table.md

Each RUNS.jsonl is an output of analyze.py. Prints markdown: every run, and
gitlet against plain git per scenario.
"""

import collections
import json
import statistics
import sys

SCENARIOS = ["signature", "samefunc", "duplicate", "independent"]


def both(r, k):
    return sum((r[a] or {}).get(k, 0) or 0 for a in "ab")


def load(paths):
    runs = {}
    for p in paths:
        for line in open(p):
            r = json.loads(line)
            runs[r["run"]] = r
    return [runs[k] for k in sorted(runs)]


def row(r):
    c = r["check"]
    ok = c["build"] and c["vet"] and c["tests"] and c["acceptance"] and c["check"]
    return {
        "run": r["run"],
        "scenario": r["scenario"],
        "mode": r["mode"],
        "ok": ok,
        "landed": all((r[a] or {}).get("landed") for a in "ab"),
        "makespan": r["makespan"],
        "agentSecs": round(both(r, "secs"), 1),
        "outputTokens": both(r, "outputTokens"),
        "contextTokens": both(r, "contextTokens"),
        "turns": both(r, "turns"),
        "toolCalls": both(r, "toolCalls"),
        "gitletCalls": both(r, "gitletCalls"),
        "gitletChars": both(r, "gitletCmdChars") or both(r, "gitletChars"),
        "gitletSecs": round(both(r, "gitletSecs"), 1),
        "rejected": both(r, "rejectedPushes"),
        "conflicts": both(r, "conflictMarkers"),
        "failed": both(r, "failedCommands"),
    }


def main():
    rows = [row(r) for r in load(sys.argv[1:])]
    print("| run | main passes | makespan s | agent s | output tokens | context tokens | turns | tool calls | gitlet calls | gitlet s | output of gitlet commands, chars | rejected pushes | git CONFLICT lines | failed commands |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for x in rows:
        print(f"| {x['run']} | {'yes' if x['ok'] else 'NO'} | {x['makespan']} | {x['agentSecs']} | {x['outputTokens']} | {x['contextTokens']} | {x['turns']} | {x['toolCalls']} | {x['gitletCalls']} | {x['gitletSecs']} | {x['gitletChars']} | {x['rejected']} | {x['conflicts']} | {x['failed']} |")
    by = collections.defaultdict(list)
    for x in rows:
        by[(x["scenario"], x["mode"])].append(x)
    print()
    print("| scenario | runs | makespan s, git | makespan s, gitlet | gitlet/git | output tokens, git | output tokens, gitlet | gitlet/git | git CONFLICT lines git / gitlet | rejected pushes git / gitlet |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for s in SCENARIOS:
        g, l = by.get((s, "git"), []), by.get((s, "gitlet"), [])
        if not g or not l:
            continue
        mean = lambda xs, k: statistics.mean(x[k] for x in xs)
        print(f"| {s} | {len(g)} + {len(l)} | {mean(g, 'makespan'):.0f} | {mean(l, 'makespan'):.0f} | {mean(l, 'makespan') / mean(g, 'makespan'):.2f} | "
              f"{mean(g, 'outputTokens'):.0f} | {mean(l, 'outputTokens'):.0f} | {mean(l, 'outputTokens') / mean(g, 'outputTokens'):.2f} | "
              f"{sum(x['conflicts'] for x in g)} / {sum(x['conflicts'] for x in l)} | {sum(x['rejected'] for x in g)} / {sum(x['rejected'] for x in l)} |")


if __name__ == "__main__":
    main()
