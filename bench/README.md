# gitlet benchmarks

This module measures whether gitlet makes many agents working on one
repository cheaper than plain git: fewer conflicts and build breaks, less
work redone or thrown away, a shorter time to land a backlog, and what gitlet
itself costs. It is a separate Go module so that its dependencies and its test
fixtures stay out of gitlet's own build.

There are two parts, because neither answers the question alone.

- **The simulation** (`gitlet-bench`) runs scripted agents on a real Go
  repository, with real git, the real gitlet binary and a merge bot that runs
  a real build. The agents do the same backlog in every mode, so the
  difference between runs is the coordination alone. It can run many agents,
  many seeds and many backlogs, but its agents act on gitlet's notes the way a
  script says, not the way a model reads them.
- **The agent study** (`llm/`) puts two real Claude Code agents on one small
  service at the same time, twice with plain git and twice with gitlet for
  each scenario, and judges what lands on main with hidden acceptance tests.
  It measures what gitlet costs a model in time and tokens and whether models
  act on its notes, but only for a handful of two-agent scenarios.

The results are in [results/RESULTS.md](results/RESULTS.md).

## The simulation

### The repository and the backlog

The base is the Go MCP SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0,
145 Go files and 57k lines), copied as one commit. `gitlet-bench gen`
makes a backlog of tasks against it. A task is one to five edits ("ops") on
real functions (two or three in most tasks, 2.5 on average), with an intent
written the way an issue would be:

| op | what it does | drawn | realized |
| --- | --- | --- | --- |
| body insert | adds statements to a function body | 24% | 36% |
| body wrap | wraps part of a body in a new block | 14% | 14% |
| body literal | changes a literal in a body | 10% | 9% |
| add function | adds a new function to a file | 16% | 18% |
| add call | makes a function call another one | 14% | 13% |
| add parameter | adds a parameter to a function and updates every caller | 16% | 10% |
| rename | renames a function and every use of it | 6% | 0.4% |

A drawn kind that does not fit its function (a signature change whose callers
cannot all be found, a rename of an unexported function or a method, a call
with nothing to call) becomes a body insert, so renames are almost absent and
about one op in ten changes a signature. The realized shares are over the
1,425 ops of the nine backlogs below.

Targets are drawn from a Zipf distribution over files (ranked by size, with
noise), and within a file by body length. A share of the draws comes instead
from a hot set: the twelve largest functions of the most heavily weighted
files. A task's further ops mostly stay in its first op's file (60%) or
directory (25%). That gives three contention profiles:

| profile | Zipf exponent | share from the hot set |
| --- | --- | --- |
| spread | 1.0 | 0 |
| clustered | 1.3 | 0.3 |
| hot | 1.6 | 0.6 |

About 8% of tasks (1 to 9 per 64-task backlog) duplicate an earlier task
within six places of the backlog. Each keeps the original's intent word for
word with probability one half (15 of the 44 did) and otherwise gets a
paraphrase, and each makes a different but equivalent edit. Every
task is checked alone against the base and dropped if the library packages
do not build (`go build`; with `--check build`, test files are not compiled),
and a duplicate whose original was dropped is dropped too. Each profile has
three backlogs (seeds 1 to 3) of 64 tasks; a run with N agents takes the
first 4N.

### Agents, rounds and the merge bot

Each agent has its own clone of a shared bare remote. Time moves in rounds; a
round is one step of an agent's work, roughly a minute of a real agent's
time. An op takes one to a few steps (an insert of k statements takes k), and
the agent commits after each one. When its task is done the agent rebases onto
main, builds, and submits a pull request. The merge bot waits two rounds for
CI, then merges in submission order with `git merge-tree`: a textual conflict
or a failing build sends the pull request back, otherwise main gets a merge
commit. CI builds the library packages only; no tests run.

A conflict is resolved the way an agent would resolve it: the agent rebuilds
its branch as the new main plus every op it has done so far. A clean rebase
that does not build (a caller of a function whose signature changed, say) is
fixed the same way, which updates the calls. Each costs a round plus a round
per 20 lines (the lines inside the conflict markers, or the lines the fix
changed), and an agent gives up on a task after six build breaks. Resolution
is always correct.

### Modes

| mode | what the agents do |
| --- | --- |
| `git` | plain git: work on a branch, rebase only when submitting |
| `gitsync` | plain git, rebasing onto main whenever it moves |
| `filelock` | advisory file locks: before an op, wait until no earlier task in flight plans to touch its files |
| `gitlet` | the real gitlet binary: `start` each task with its intent and the files it plans to touch (`--paths`, the same lists filelock locks on), `status` every round, act on notes, `submit` |

The gitlet agents follow a scripted policy built on the skill. Every round
they run `gitlet status`, and before an op on an existing function they run
`gitlet who` on that function. They read warn and act notes, and info notes
only when they are duplicate notes. The waiting rules below are the
benchmark's, not gitlet's: the skill calls a warn note "a risk to keep in
mind", and overlap notes offer only `discuss` and `dismiss`, which the
scripted agents never use.

- Defer (the default `--overlap defer`): when a warn or act `body_overlap` or
  `predicted_conflict` note names an earlier task, or `who` lists an earlier
  task in flight as changing the function, the later task puts the op off
  until the earlier one lands or stops, or is blocked on a contract, for at
  most about 25 rounds, and does its other ops meanwhile. An agent with
  nothing left to do waits; it does not take another task. `--overlap notes`
  defers on the notes but not on `who`, and `--overlap ignore` never defers.
- `callee_changed`: `need` the symbol; `ready` and `drift`: `use` the stub,
  adapting its calls; `callers_affected`: `claim` and `publish` a stub;
  `needed`, `diverged`, `agreement`: `publish`; `message`: `accept`;
  `still_waiting`: `release`. A finished task whose `submit` is refused
  because a contract it used has not landed waits for it; after 30 rounds it
  releases its needs and rebuilds on main's signatures.
- `signature_clash`: the later task takes its own change of the signature
  out, records a `need`, and redoes its change on top once the earlier task
  lands. If the clash arrives after its ops are done, the change is dropped
  (this happened once).
- `stale` and `new_callers`: bring main in and fix calls.
- `duplicate`: the later task abandons itself on every act-severity duplicate
  note. On an info or warn one it abandons only when the other task really is
  a copy (the benchmark knows which), which is an upper bound on what a model
  would do with those notes. The `actdup` variant (`--dup-judge act`)
  abandons only on act-severity duplicate notes.

Every gitlet call but `status`, `start`, `update` and `submit` (which stand in
for git commands the other modes run anyway) costs the agent a quarter of a
round; reading notes costs nothing. A gitlet round takes at least six seconds
of wall time, and gitlet's timeouts are scaled to that, so that its
background snapshots and remote polling run as they would. With 8 and 16
agents, rounds took 6 to 21 s because of builds and gitlet's own calls, so
thread and publish timeouts lasted fewer rounds than intended at 16 agents.

### What is measured

Per run (`runs/NAME/result.json` and `events.jsonl`):

- **rounds**: the makespan, the number of rounds (from round 0) until every
  task had ended, landed or dropped; normally the round of the last landing
  plus one, and 401 for a run stopped at the cap.
- **conflicts** and **conflict lines**: conflicts the agents resolved by hand.
- **build breaks** and **CI failures**: local builds broken after a clean
  rebase, and pull requests CI sent back.
- **rework lines**: conflict lines, lines changed fixing builds, lines changed
  adapting to a gitlet stub or to main after a `stale` or `new_callers` note,
  lines taken out to wait, work thrown away when a task is abandoned as a
  duplicate or finds its duplicate already landed, and the lines of a
  duplicate that landed next to its original.
- **busy, wait and blocked agent-rounds**: rounds an agent spent resolving,
  fixing, adapting or coordinating; rounds it had nothing it could do
  (deferred or locked out); and rounds a finished task waited for a contract
  to land.
- **main vet**: whether the final main passes `go vet ./...`, which also
  compiles the tests.
- **gitlet's cost**: calls, notes read, note characters, seconds in gitlet.

`gitlet-bench oracle` then judges the notes. It applies each task alone to
the base and merges every pair, building the clean merges that can break (one
task changes a signature the other calls, or both change signatures): a pair
interacts if it conflicts textually, merges cleanly but does not build, or is
a duplicate. For pairs that were in flight at the same time it reports how
many interacting pairs a warn or act note named (recall), how many warned
pairs really interact (precision), and how many rounds of warning there were
before the first of the two landed. Ground truth is against the base, not the
main each task really started from, so it misses interactions that only
appear on a later main, including conflicts gitlet's own stubs cause; and as
only builds run, it has no semantic interactions.

### Running it

The work directory must be an absolute path.

```
go build -o gitlet-bench ./cmd/gitlet-bench
make -C .. build                       # gitlet itself, into ../bin/gitlet
GOFLAGS=-mod=mod go mod download github.com/modelcontextprotocol/go-sdk@v1.8.0
SRC=$(go env GOMODCACHE)/github.com/modelcontextprotocol/go-sdk@v1.8.0
W=$PWD/W
for p in spread clustered hot; do for s in 1 2 3; do
  ./gitlet-bench gen --work "$W" --target "$SRC" --profile $p --seed $s --tasks 64 --check build
done; done
WORK="$W" GITLET=$PWD/../bin/gitlet BENCH=$PWD/gitlet-bench ./matrix.sh run
for p in spread clustered hot; do for s in 1 2 3; do
  ./gitlet-bench oracle --work "$W" --tasks "$W/tasks-$p-$s.json" --limit 64 "$W"/runs/$p-s$s-n*-gitlet*/ > oracle-$p-$s.json
done; done
python3 summarize.py "$W" oracle-*.json
```

`WORK="$W" ./matrix.sh list` prints the 132 runs: three profiles, three
seeds, 4, 8 and 16 agents and four modes (108), plus gitlet policy variants
(24). At 8 agents: `ignore` on the clustered and hot backlogs, and on the hot
backlogs `half` (acts on each warn or act note with probability one half; the
`who` deferral is unchanged) and `actdup`. At 16 agents: `ignore` on every
profile and `notes` on the hot backlogs. On four CPUs, with up to three runs
at a time, git and gitsync runs took 1 to 17 minutes, filelock runs 1 to 6
and gitlet runs 5 to 30. Most of it is Go builds; for gitlet it is the
six-second round at 4 and 8 agents and gitlet's own calls (about 4 s each)
at 16 agents. Runs stop at 400 rounds. The oracle's ground truth for a
64-task backlog covers runs of every size on it, and takes about ten
minutes, as only the pairs that can break the build are built.

## The agent study

`llm/testdata/fixture` is kvsvc, a small key-value service (a store, an HTTP
server and client, a config loader, two commands). Each scenario gives two
agents tasks that interact in a known way:

| scenario | agent a | agent b | how they interact |
| --- | --- | --- | --- |
| signature | add a read-only mode, changing `store.New`'s signature | add `store.FromPairs`, which calls `store.New` | b's new code breaks once a lands |
| samefunc | `store.Put` rejects empty keys | `store.Put` enforces the key limit | both edit the same function |
| duplicate | add retries to `client.Fetch` | the same, worded differently | one is wasted work |
| independent | a `-version` flag | `store.Len` | none: the control |

`llm/run.sh setup DIR SCENARIO MODE` makes a bare remote and two clones. In
gitlet mode it runs `gitlet init --client claude-code --git-hooks` in each
clone and removes the client files, so that only the git hooks remain, and
writes a wrapper that logs every call into `DIR/bin-a` and `DIR/bin-b`, which
each agent's command prefix puts first on PATH.

`llm/prompt.py` writes the prompt from `llm/prompts/`: the task; the rules
(land on origin/main, build, vet and tests must pass on top of it, never
force-push, no need to land a copy of work the other agent already landed);
a landing procedure for the mode (git: fetch, rebase or merge, push to main;
gitlet: run `gitlet status` after each commit and before landing, then
`gitlet submit`, `gitlet update`, the checks and a push to main, then a final
`gitlet status`); and in gitlet mode gitlet's skill file. The two agents are
Claude Code subagents started at the same moment by a workflow, one run at a
time.

`llm/run.sh check DIR SCENARIO` then clones main, runs build, vet and tests,
copies in the hidden acceptance tests from `llm/testdata/acceptance/` and
runs them, plus the scenario's extra check (the `-version` output for
independent), and prints the verdict as JSON. `llm/analyze.py RUNS_DIR
WORKFLOW_DIR...` reads the agents' transcripts and gitlet's logs for time,
turns, tokens, tool calls, gitlet calls and their time, the output of
commands that call gitlet, rejected pushes and git's `CONFLICT` lines, runs
the check on each run, and prints one JSON line per run; `llm/table.py` turns
that into the tables in results/agent-tables.md.

In this sandbox there is no MCP server and no pull-request server: the agents
call the gitlet CLI and land by pushing to main, so notes reach them only in
gitlet's own results, and act notes also after `git commit` (the post-commit
hook).
