"""Summarize agent-study runs: analyze.py RUNS_DIR WORKFLOW_DIR... > runs.jsonl

RUNS_DIR holds the run directories made by run.sh setup. Each WORKFLOW_DIR is
the transcript directory of a workflow that ran agents labelled RUN:a and
RUN:b. For each run it prints one JSON line: run.sh check's verdict on main,
and per agent its wall time, tokens, tool calls, gitlet calls and output,
and the git trouble it met (conflicts, rejected pushes, failed commands).
"""

import collections
import datetime
import json
import os
import re
import subprocess
import sys

here = os.path.dirname(os.path.abspath(__file__))

# A shell command that runs gitlet: the wrapper on PATH, or the old
# per-agent wrapper path of the first pass.
GITLET_CMD = re.compile(r"(?<![\w/.-])gitlet\s+[a-z-]|bin/gitlet-[ab]\s")


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def text_of(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(c.get("text", "") for c in content if isinstance(c, dict))
    return ""


def agent_stats(path):
    lines = [json.loads(l) for l in open(path)]
    stamps = [ts(l["timestamp"]) for l in lines if "timestamp" in l]
    seen = set()
    out_tok = ctx_tok = turns = 0
    tools = collections.Counter()
    cmds = {}
    for l in lines:
        if l.get("type") != "assistant":
            continue
        m = l["message"]
        if m.get("id") not in seen:
            seen.add(m.get("id"))
            u = m.get("usage") or {}
            out_tok += u.get("output_tokens", 0)
            ctx_tok += u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0) + u.get("cache_creation_input_tokens", 0)
            turns += 1
        for c in m.get("content", []):
            if c.get("type") != "tool_use":
                continue
            tools[c["name"]] += 1
            cmd = (c.get("input") or {}).get("command", "")
            cmds[c["id"]] = cmd
    conflicts = rejected = errors = gitlet_errors = gitlet_chars = 0
    for l in lines:
        if l.get("type") != "user" or not isinstance(l["message"].get("content"), list):
            continue
        for c in l["message"]["content"]:
            if c.get("type") != "tool_result":
                continue
            t = text_of(c.get("content"))
            conflicts += len(re.findall(r"^CONFLICT \(", t, re.M))
            rejected += t.count("[rejected]")
            gl = GITLET_CMD.search(cmds.get(c.get("tool_use_id"), "")) is not None
            if c.get("is_error"):
                if gl:
                    gitlet_errors += 1  # refusals exit 3: protocol, not trouble
                else:
                    errors += 1
            if gl:
                # The whole output of a command that calls gitlet: chained
                # commands (git commit && gitlet status) count in full.
                gitlet_chars += len(t)
    return {
        "secs": round(max(stamps) - min(stamps), 1) if stamps else 0,
        "start": min(stamps) if stamps else 0,
        "end": max(stamps) if stamps else 0,
        "turns": turns,
        "outputTokens": out_tok,
        "contextTokens": ctx_tok,
        "toolCalls": sum(tools.values()),
        "bash": tools.get("Bash", 0),
        "gitletCmdChars": gitlet_chars,
        "conflictMarkers": conflicts,
        "rejectedPushes": rejected,
        "failedCommands": errors,
        "gitletNonZero": gitlet_errors,
    }


def gitlet_log(path):
    """Reads a run's gitlet wrapper log: calls by verb, refusals, seconds."""
    verbs, codes = collections.Counter(), collections.Counter()
    hooks = 0
    secs, started = 0.0, []
    if not os.path.exists(path):
        return {"gitletCalls": 0, "gitletVerbs": {}, "gitletExit": {}, "gitletSecs": 0}
    for line in open(path):
        parts = line.rstrip("\n").split(" ")
        if len(parts) >= 3 and parts[1] == "start":
            started.append(float(parts[0]))
            # `gitlet --help` and `gitlet VERB --help` are reading the help.
            verbs["help" if "--help" in parts[2:] else parts[2]] += 1
            if parts[2] == "hook":
                hooks += 1
        elif len(parts) >= 4 and parts[1] == "exit":
            codes[parts[2]] += 1
            # Hooks run inside some calls; count the outermost call's time.
            if started:
                t0 = started.pop()
                if not started:
                    secs += float(parts[0]) - t0
    return {"gitletCalls": sum(verbs.values()) - hooks, "gitletHooks": hooks, "gitletVerbs": dict(verbs), "gitletExit": dict(codes), "gitletSecs": round(secs, 1)}


def main():
    runs_dir, wf_dirs = sys.argv[1], sys.argv[2:]
    agents = {}  # (run, id) -> stats
    results = {}
    for wf in wf_dirs:
        labels = {}
        for l in open(os.path.join(wf, "journal.jsonl")):
            j = json.loads(l)
            if j.get("type") == "started":
                labels[j["agentId"]] = j["label"]
            if j.get("type") == "result":
                results[j["agentId"]] = j["result"]
        for aid, label in labels.items():
            run, _, id = label.rpartition(":")
            p = os.path.join(wf, "agent-" + aid + ".jsonl")
            if os.path.exists(p):
                s = agent_stats(p)
                s["report"] = results.get(aid)
                agents[(run, id)] = s
    for run in sorted({r for r, _ in agents}):
        d = os.path.join(runs_dir, run)
        scenario = open(os.path.join(d, "scenario")).read().strip()
        mode = open(os.path.join(d, "mode")).read().strip()
        check = json.loads(subprocess.check_output([os.path.join(here, "run.sh"), "check", d, scenario], text=True, stderr=subprocess.DEVNULL))
        a, b = agents.get((run, "a")), agents.get((run, "b"))
        for id, x in (("a", a), ("b", b)):
            if x:
                x.update(gitlet_log(os.path.join(d, "gitlet-" + id + ".log")))
        for x in (a, b):
            if x:
                c = (x.get("report") or {}).get("commit", "")
                x["landed"] = bool(c) and subprocess.run(["git", "-C", os.path.join(d, "remote.git"), "merge-base", "--is-ancestor", c, "main"], capture_output=True).returncode == 0
        both = [x for x in (a, b) if x]
        rec = {
            "run": run,
            "scenario": scenario,
            "mode": mode,
            "check": check,
            "makespan": round(max(x["end"] for x in both) - min(x["start"] for x in both), 1),
            "a": a,
            "b": b,
        }
        print(json.dumps(rec))


if __name__ == "__main__":
    main()
