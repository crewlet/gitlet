"""Print the task prompt for one agent of a run: prompt.py DIR AGENT.

DIR is a run directory made by run.sh setup; AGENT is a or b.
"""

import json
import os
import re
import sys

here = os.path.dirname(os.path.abspath(__file__))


def read(*parts):
    with open(os.path.join(*parts)) as f:
        return f.read()


def main():
    run, agent = sys.argv[1], sys.argv[2]
    scenario = read(run, "scenario").strip()
    mode = read(run, "mode").strip()
    task = json.loads(read(here, "testdata", "acceptance", "scenarios.json"))[scenario][agent]
    skill = read(here, "..", "..", "internal", "install", "skill", "SKILL.md")
    skill = re.sub(r"\A---\n.*?\n---\n+", "", skill, flags=re.S)
    out = read(here, "prompts", "common.md").replace("{{MODE}}", read(here, "prompts", mode + ".md").rstrip("\n"))
    values = {
        "AGENT": "agent-" + agent,
        "CLONE": os.path.join(run, agent),
        "TASK": task,
        "PREFIX": "cd " + os.path.join(run, agent) + " &&"
        + (" export PATH=" + os.path.join(run, "bin-" + agent) + ":$PATH &&" if mode == "gitlet" else ""),
        "SKILL": skill.strip(),
    }
    for k, v in values.items():
        out = out.replace("{{" + k + "}}", v)
    print(out.strip())


if __name__ == "__main__":
    main()
