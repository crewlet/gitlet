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
  service at the same time, once with plain git and once with gitlet, and
  judges what lands on main with hidden acceptance tests. It measures what
  gitlet costs a model in time and tokens and whether models act on its notes,
  but only for a handful of two-agent scenarios.

The results are in [results/RESULTS.md](results/RESULTS.md).

## The simulation

### The repository and the backlog

The base is the Go MCP SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0,
about 140 Go files and 50k lines), copied as one commit. `gitlet-bench gen`
makes a backlog of tasks against it. A task is one to three edits ("ops") on
real functions, with an intent written the way an issue would be:

| op | what it does | share |
| --- | --- | --- |
| body insert | adds statements to a function body | 24% |
| body wrap | wraps part of a body in a new block | 14% |
| body literal | changes a literal in a body | 10% |
| add function | adds a new function to a file | 16% |
| add call | makes a function call another one | 14% |
| add parameter | adds a parameter to a function and updates every caller | 16% |
| rename | renames a function and every use of it | 6% |

Which functions are targeted is drawn from a Zipf distribution over files,
with a share of targets drawn from the twelve most-called functions. That
gives three contention profiles:

| profile | Zipf exponent | share from the hot set |
| --- | --- | --- |
| spread | 1.0 | 0 |
| clustered | 1.3 | 0.3 |
| hot | 1.6 | 0.6 |

About 8% of tasks duplicate an earlier task within six places of the backlog,
with a paraphrased intent and a different but equivalent edit. Every task is
checked alone against the base and dropped if it does not build. Each profile
has three backlogs (seeds 1 to 3) of 64 tasks; a run with N agents takes the
first 4N.

### Agents, rounds and the merge bot

Each agent has its own clone of a shared bare remote. Time moves in rounds; a
round is one step of an agent's work, roughly a minute of a real agent's
time. An op takes one to a few steps (an insert of k statements takes k), and
the agent commits after each one. When its task is done the agent rebases onto
main, builds, and submits a pull request. The merge bot waits two rounds for
CI, then merges in submission order with `git merge-tree`: a textual conflict
or a failing build sends the pull request back, otherwise main gets a merge
commit.

A conflict is resolved the way an agent would resolve it: it rebases, and for
each conflicted op it redoes the op on the new main. A clean rebase that does
not build (a caller of a function whose signature changed, say) is fixed by
updating the calls. Both cost the agent rounds at 20 changed lines a round.

### Modes

| mode | what the agents do |
| --- | --- |
| `git` | plain git: work on a branch, rebase only when submitting |
| `gitsync` | plain git, rebasing onto main whenever it moves |
| `filelock` | advisory file locks: before an op, wait until no earlier task in flight touches its files |
| `gitlet` | the real gitlet binary: `start` each task, `status` every round, act on notes, `submit` |

The gitlet agents follow the skill: every round they run `gitlet status` and
act on what the notes say, and before an op on a function they run
`gitlet who` on its type or file.

- `body_overlap` and `predicted_conflict`: the later of the two tasks puts the
  overlapping op off until the earlier one lands (at most 25 rounds), and
  does its other ops meanwhile.
- `callee_changed`: `need` the symbol; `ready` and `drift`: `use` the stub,
  adapting its calls; `callers_affected`: `claim` and `publish` a stub;
  `needed`, `diverged`, `agreement`: `publish`.
- `signature_clash`: the later task drops its own signature change of the
  symbol and adopts the earlier one's.
- `stale` and `new_callers`: bring main in and fix calls.
- `duplicate`: the later task abandons itself. By default the agent judges
  each duplicate note rightly (it knows whether the other task really is a
  copy), which is an upper bound on what a model would do; the `actdup`
  variant abandons only on gitlet's act-severity duplicate notes.

Every gitlet call but `status`, `start`, `update` and `submit` (which stand in
for git commands the other modes run anyway) costs the agent a quarter of a
round. A gitlet round takes at least six seconds of wall time, so that
gitlet's background snapshots and remote polling run as they would.

### What is measured

Per run (`runs/NAME/result.json` and `events.jsonl`):

- **rounds**: the makespan, the round the last task landed.
- **conflicts** and **conflict lines**: conflicts the agents resolved by hand.
- **build breaks** and **CI failures**: local builds broken after a clean
  rebase, and pull requests CI sent back.
- **rework lines**: conflict lines, lines changed fixing builds, lines changed
  adapting to a gitlet stub, lines taken out to wait, and duplicate work
  thrown away.
- **wait and blocked agent-rounds**: rounds an agent had nothing it could do
  (deferred or locked out) or waited for a contract to land.
- **gitlet's cost**: calls, notes read, note characters, seconds in gitlet.

`gitlet-bench oracle` then judges the notes. It applies each task alone to
the base and merges every pair: a pair interacts if it conflicts textually,
merges cleanly but does not build, or is a duplicate. For pairs that were in
flight at the same time it reports how many interacting pairs a warn or act
note named (recall), how many warned pairs really interact (precision), and
how many rounds of warning there were before the first of the two landed.
Ground truth is against the base, not the main each task really started
from, so it misses interactions that only appear on a later main.

### Running it

```
go build -o gitlet-bench ./cmd/gitlet-bench
make -C .. build                       # gitlet itself, into ../bin/gitlet
GOFLAGS=-mod=mod go mod download github.com/modelcontextprotocol/go-sdk@v1.8.0
SRC=$(go env GOMODCACHE)/github.com/modelcontextprotocol/go-sdk@v1.8.0
for p in spread clustered hot; do for s in 1 2 3; do
  ./gitlet-bench gen --work W --target "$SRC" --profile $p --seed $s --tasks 64 --check build
done; done
WORK=W GITLET=$PWD/../bin/gitlet BENCH=$PWD/gitlet-bench ./matrix.sh run
./gitlet-bench oracle --work W --tasks W/tasks-hot-1.json --limit 32 W/runs/hot-s1-n8-* > oracle-hot-1-8.json
python3 summarize.py W oracle-*.json
```

`matrix.sh list` prints the 120 runs: three profiles, three seeds, 4, 8 and 16
agents, four modes, and policy variants of gitlet at 8 agents (`ignore` goes
on despite overlap notes, `half` acts on each note with probability one half,
`actdup` as above). Plain-git runs take about a minute each; gitlet runs take
five to fifteen minutes, because of the six-second round.

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

`llm/run.sh setup DIR SCENARIO MODE` makes a bare remote and two clones; in
gitlet mode it runs `gitlet init --git-hooks` in each and puts a wrapper that
logs every call first on the agent's PATH. `llm/prompt.py` writes the prompt
from `llm/prompts/`: the task, the rules (land on origin/main, checks must
pass on top of it, never force-push), and in gitlet mode gitlet's own skill
file. The two agents are Claude Code subagents started at the same moment.
`llm/run.sh check DIR SCENARIO` then clones main, runs build, vet and tests,
copies in the hidden acceptance tests from `llm/testdata/acceptance/`, and
reports whether they pass. `llm/analyze.py` reads the agents' transcripts and
gitlet's logs for time, turns, tokens, tool calls, gitlet calls, rejected
pushes and conflict markers.

In this sandbox there is no MCP server and no pull-request server: the agents
call the gitlet CLI and land by pushing to main, so notes reach them only in
gitlet's own results and after `git commit`.
