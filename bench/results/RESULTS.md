# Does gitlet win anything? Results

Measured on 2026-10-26 and 27 against gitlet b16f0c3. How the benchmarks
work is in [../README.md](../README.md); the full generated tables are in
[sim-tables.md](sim-tables.md) and [agent-tables.md](agent-tables.md), and the
raw numbers in [runs.csv](runs.csv), [oracle/](oracle) and
[agents.jsonl](agents.jsonl).

## The short answer

No. In neither benchmark was gitlet clearly faster or cheaper than plain git.
In the simulation, under the benchmark's waiting policy, it was slower in 19
of 27 paired runs; without that waiting it was even with plain git (0.95 to
1.06 by cell). With two real agents, every gitlet run was slower. gitlet did
two things better: agents built the same task twice less often, and at 16
agents there were fewer conflicts. Neither paid for what gitlet cost here,
for two reasons.

- **There was little to win, under the simulation's cost model.** The
  simulation charges a conflict or build break a round plus a round per 20
  lines, always resolved correctly and with no tests run, and charges each
  gitlet coordination call a quarter round. None of these costs was measured
  or varied, and the verdict rests on them. Under that model, plain git's
  agents lost 0.4% to 5% of their time to conflicts and build fixes at 4 and
  8 agents and 1.3% to 11.6% at 16, counting the extra CI cycle of a pull
  request sent back: 0.04 to 0.6 rounds per task, and 2.1 at 16 agents on the
  hot backlogs. gitlet's coordination calls alone were charged about half a
  round per task, more than that in 7 of the 9 cells. Duplicate tasks cost
  plain git more than conflicts did on the spread and clustered backlogs
  (2.9% to 6.5% of agent time there at 8 and 16 agents), and the simulated gitlet
  agents saved that only by judging duplicate notes with ground truth. With
  real agents, git's worst interaction threw away one agent's 91 to 99
  seconds of work; gitlet added 19 to 124 seconds of makespan to each run
  (30 to 99 on average per scenario), 30 of them on average where the agents
  never touched the same code.
- **Waiting.** The simulated gitlet agents put off an op that an earlier task
  is changing until that task lands. That is the benchmark's policy, not
  gitlet's: the skill calls a warn note "a risk to keep in mind", overlap
  notes offer only `discuss` and `dismiss`, and the real agents kept working
  on them. The waiting is most of gitlet's extra makespan. Without deferring
  on notes or `who`, gitlet was 1.01 to 1.06 times plain git at 8 agents and
  0.95 to 1.02 at 16 (faster in 7 of 15 paired runs, slower in 5), but with
  as many conflicts as plain git or more (6.7 against 5.3 at 8 agents on
  hot). Only the lead at spread 16 (0.95) is more than a round, and it most
  likely owes that to duplicate tasks the benchmark's ground-truth judge
  dropped early.

In more detail:

- **Throughput.** In the simulation, under the defer policy, gitlet's
  makespan was 0.99 to 1.38 times plain git's on average (0.94 to 1.59 in
  single runs). It was slower in 19 of the 27 paired runs (nine backlogs at
  three team sizes), equal in 5, and faster in 3, by 1 to 3 rounds. It did
  worst at 16 agents on the hot backlogs (1.38), where its agents waited 374
  agent-rounds per run. With two real Claude agents it took 1.5 to 2.4 times
  as long, and every gitlet run was slower than both git runs of its
  scenario. The overhead there was 30 s with no interaction and 40 to 100 s
  with one, on tasks plain git landed in about a minute. The protocol part
  may be roughly fixed per task, so its share would shrink on longer tasks;
  the waiting part would grow with them, since in the signature runs one
  agent could not land until the other had. The study did not measure longer
  tasks.
- **Cost.** Real agents with gitlet took 1.8 times the turns and output tokens
  and twice the input tokens of plain git, for the same result: every run in
  both modes landed code that passes the hidden acceptance tests. The extra
  tokens follow the extra turns. Output per turn was the same in both modes,
  and every extra turn re-sends the agent's context (about 38k tokens at the
  start), which is two thirds or more of the extra input tokens; gitlet's own
  output is a few percent of it. gitlet itself was fast with two agents: a
  median of 0.3 s a call (90% under 0.81 s), 89 s over the 230 calls of all
  eight gitlet runs. Part of the extra is this sandbox's procedure: with no
  MCP server, client hooks or pull-request server, the agents were told to
  run `gitlet status` after every commit, before landing and at the end, and
  to land by `submit`, `update` and a push of their own.
- **Duplicated work: less of it, but no saving.** In both real-agent
  duplicate runs with git, one agent wrote, committed and threw away a full
  implementation (167 and 179 lines). With gitlet neither run wasted code,
  because `gitlet status` listed the other agent's task; gitlet raised no
  duplicate note (the two wordings scored 0.31 and 0.32 against its 0.5
  threshold). It saved no time or tokens: the duplicate scenario took 1.76
  times as long with gitlet. The agent that stepped aside used more time and
  tokens waiting (152 to 184 s, 6.7k to 8.6k output tokens) than git's agent
  spent writing its throwaway copy (91 to 99 s, 6.0k to 6.5k), and the agent
  that did the work took 133 to 144 s against git's 71 to 73. Each agent here
  had one task and was done only once it was on main, so the agent that
  stepped aside could only poll main until the other landed; with other work
  to take, as the skill says, that time would not be lost, while git's
  throwaway copy is. In the simulation gitlet avoided 23 to 123 lines of
  duplicate work per run at 8 and 16 agents, but only because the scripted
  agents judge every info duplicate note with ground truth. Acting only on
  gitlet's act duplicate notes (8 agents, hot), 2 of 3 duplicates landed next
  to their originals and 1 was found late; with plain git all 3 were found
  late. Duplicate notes named every true duplicate pair in flight, among 3 to
  22 times as many false ones. Of the 1,110 duplicate notes the agents read,
  all were at info severity but one, and that act note was wrong and dropped
  a real task.
- **Conflicts: consistent at 16 agents, but cheap here.** At 16 agents gitlet
  had fewer conflicts than plain git on 8 of 9 backlogs (1.3 to 3.7 fewer per
  run, 15% to 39%), and 13% to 20% fewer than rebasing whenever main moves. At
  8 agents the differences are within backlog-to-backlog noise, and rebasing
  whenever main moves did as well. The fewer conflicts come from the waiting:
  without it, gitlet had about as many as git (3.7, 8.3 and 26.0 per run at
  16 agents on spread, clustered and hot, against git's 3.3, 9.0 and 25.0). A
  conflict here costs an agent about two rounds, and about three once the
  extra CI cycle of a pull request sent back is counted. With real agents on
  the same function, `who` before editing listed nobody, and gitlet warned
  only once both had committed; in one run the warning came 4 s before the
  first agent landed and the agents' thread started after that. They settled
  that whoever landed second would merge, which is what happens with plain
  git, and the second agent resolved the conflict by hand, with plain git,
  because `gitlet update` refused.
- **The contract flow: its stubs do not build.** A stub that gitlet builds for
  a breaking signature change replaces only the function in main's file, so
  the updates to the function's existing callers and any new declarations its
  body uses are missing. Both real signature runs hit this: the stub failed
  with "unknown field readOnly", and with that field added it would still
  fail, because main's caller passes the old arguments. The owner republished
  its whole commit as the stub (`publish --commit`), and the other agent had
  to wait for the owner to land. With plain git the agent whose rebase broke
  the build fixed it in one small commit. In the simulation, on the hot
  backlogs, adapting to stubs and updates cost more lines (11 to 65 per run)
  than the build fixes saved (0 to 32), much of it because the scripted
  agents follow every act `callee_changed` note, even in tasks that only call
  the changed function; the time it cost was small (at most 5.7 agent-rounds
  blocked per run, at 4 agents, and 2.3 at 16).
- **Most notes name pairs that do not interact.** At 8 and 16 agents, warn
  and act notes named 27% to 58% of the conflicting or build-breaking task
  pairs in flight together, and 16% to 44% of the pairs they named really
  conflict, break the build or duplicate. Counted over every task a note
  names, info notes included, 72% to 88% of the tasks named do not interact
  (73% to 87% counting each distinct note once). The warnings already come
  from `git merge-tree`. The truth here is each task applied alone to the
  base, so it counts as false the conflicts that only arise during a run,
  including ones gitlet's own stubs caused; that overstates the noise by an
  amount this benchmark does not measure.
- **File locks, as tested, are much worse.** Locking every file a task plans
  to touch until the task lands avoided textual conflicts (not build breaks)
  but took 1.2 to 6.4 times plain git's makespan (1.1 to 7.4 in single runs;
  two of three runs at 16 agents on hot hit the 400-round cap), and it
  landed 64 of 65 duplicate tasks next to their originals. Shorter or
  finer-grained locks were not tested.

## How sure these numbers are

The largest uncertainty is the cost model above: what a conflict, a build
break and a coordination call cost is assumed, not measured. At 4 agents,
gitlet's slowdown of 1 to 3 rounds is about the quarter-round charge for its
coordination calls (about two rounds per agent), so there the direction is
set by that assumption.

Beyond that, each simulation cell is one run on each of three backlogs, and
no configuration was repeated, so run-to-run noise is not measured; gitlet
runs depend on wall-clock timing. The spread between backlogs is large:
gitlet/git ranged 1.00 to 1.35 at 8 agents on hot and 1.15 to 1.59 at 16.
With three backlogs, the 95% interval of a cell's mean ratio includes 1 in
every cell but spread at 4 agents. The 27 paired runs are nine backlogs at
three team sizes (the 4- and 8-agent runs take the first 16 or 32 tasks of
the same 64), so they are not independent. Summed over the three sizes,
gitlet under the defer policy was slower on 8 of the 9 backlogs (a sign test
gives p ≈ 0.04), and it had fewer conflicts in 16 of the 27 runs and more in
2. Without the waiting there is no direction: of 15 paired runs, gitlet was
faster in 7, equal in 3 and slower in 5. One backlog (hot, seed 1, 16 agents)
holds most of its cell's conflicts and build breaks: git had 41 conflicts and
61 build breaks or CI failures there, against 12 to 22 and 6 to 7 on the
other two. In the agent study each cell has two runs, but every gitlet run
took longer and used more output tokens than both git runs of its scenario.

## The simulation

Scripted agents work through the same backlog in each mode, on the Go MCP
SDK, with real git, real merges, real builds and the real gitlet binary (see
the README). Each cell is the mean of three backlogs (seeds), 4N tasks for N
agents. A round is about a minute of agent work.

### Makespan

Rounds until every task had ended.

| profile | agents | git | gitsync | filelock | gitlet | gitlet / git, mean of per-backlog ratios (range) |
|---|---|---|---|---|---|---|
| spread | 4 | 45.7 | 45.7 | 56.0 | 48.0 | 1.05 (1.04-1.06) |
| spread | 8 | 46.3 | 45.0 | 110.3 | 46.0 | 0.99 (0.94-1.07) |
| spread | 16 | 52.3 | 52.3 | 201.3 | 52.0 | 0.99 (0.98-1.00) |
| clustered | 4 | 45.3 | 45.3 | 79.0 | 46.7 | 1.03 (1.00-1.05) |
| clustered | 8 | 51.7 | 51.7 | 144.3 | 57.0 | 1.11 (1.05-1.21) |
| clustered | 16 | 53.7 | 53.3 | 243.3 | 57.3 | 1.07 (1.04-1.10) |
| hot | 4 | 46.0 | 46.0 | 103.3 | 50.7 | 1.11 (1.00-1.30) |
| hot | 8 | 50.7 | 50.7 | 195.7 | 60.0 | 1.19 (1.00-1.35) |
| hot | 16 | 65.0 | 64.3 | 396.0* | 90.3 | 1.38 (1.15-1.59) |

\* Two of the three runs hit the 400-round cap with tasks unfinished.

### Conflicts and rework

Conflicts resolved by hand per run, and the lines of rework.

| profile | agents | conflicts git / gitsync / gitlet | conflict lines git / gitlet | build-fix lines git / gitlet | stub and update lines, gitlet | duplicate lines git / gitlet |
|---|---|---|---|---|---|---|
| spread | 8 | 0.7 / 0.7 / 0.3 | 4.0 / 3.3 | 0 / 0 | 0 | 44.0 / 0 |
| spread | 16 | 3.3 / 2.3 / 2.0 | 38.3 / 19.3 | 1.3 / 0.7 | 0 | 119.7 / 1.0 |
| clustered | 8 | 2.7 / 2.0 / 2.3 | 22.3 / 22.3 | 2.0 / 0.7 | 0.7 | 40.7 / 4.7 |
| clustered | 16 | 9.0 / 8.3 / 6.7 | 118.3 / 81.7 | 4.7 / 18.3 | 0 | 128.0 / 5.0† |
| hot | 8 | 5.3 / 3.3 / 3.7 | 62.7 / 43.0 | 5.3 / 4.0 | 42.0 | 22.7 / 0 |
| hot | 16 | 25.0 / 26.7 / 21.3 | 321.3 / 286.3 | 60.3 / 28.7 | 65.3 | 102.0 / 2.0 |

Duplicate lines are the work thrown away when a duplicate task is abandoned
or finds its copy already landed, plus the lines of duplicates that landed
next to their originals (git landed 8 such duplicates over all runs, gitsync
11, gitlet 7 and file locks 64). Stub and update lines are calls changed
after `use` of a stub, or after `update` on a `stale` or `new_callers` note.
† Includes 9 lines of a real task dropped on a wrong act duplicate note.

Most of gitlet's lower rework is duplicate work, avoided thanks to the
ground-truth judge. Without it, gitlet's rework is lower than git's at 16
agents on the spread and clustered backlogs (20 against 40 and 100 against
123 lines per run) and about the same at 8 agents there. On the hot backlogs
it is higher or the same (89 against 68 at 8 agents, 386 against 382 at 16),
because stub lines offset its fewer conflict lines. Rebasing whenever main
moves had less of it than gitlet at 8 agents (17 against 24 lines on
clustered, 49 against 89 on hot). At 4 agents there is almost no contention
in any mode (see sim-tables.md).

### Where the time goes

Agent-rounds per run. Busy is resolving conflicts, fixing builds, adapting
and, for gitlet, coordination calls at a quarter round each; it leaves out
the first round of each conflict or build break and the extra CI cycle of a
pull request sent back, which the per-task figures in the short answer
count. Waiting is having nothing runnable because ops were deferred; blocked
is a finished task waiting for a contract to land.

| profile | agents | busy, git | busy, gitlet | waiting, gitlet | blocked, gitlet | waits caused by `who` |
|---|---|---|---|---|---|---|
| spread | 16 | 4.3 | 28.0 | 24.0 | 0 | 63% |
| clustered | 8 | 3.7 | 16.0 | 23.7 | 0 | 38% |
| clustered | 16 | 11.7 | 33.0 | 57.0 | 0 | 80% |
| hot | 8 | 8.3 | 24.3 | 51.7 | 2.0 | 52% |
| hot | 16 | 43.7 | 67.0 | 374.0 | 2.3 | 51% |

Under the benchmark's defer policy, the later of two tasks puts off an op
that an earlier task is changing, when a warn or act overlap note names it or
`gitlet who` lists it, until the earlier one lands (at most about 25 rounds).
The last column is the share of deferred op-rounds that `who` caused rather
than a note; nearly all the rest came from `predicted_conflict` notes. At 16
agents on hot the waiting was 136 to 576 agent-rounds in single runs. On the
hot backlogs gitlet's extra rounds come from waiting; at spread 16 it waited
and coordinated about equally and was no slower.

Coordination calls cost about half a round per task: about two calls per
task beyond `status`, `start`, `update` and `submit`, at a quarter round each,
which is the simulation's assumption, not a measurement. Plain git lost 0.04
to 0.6 rounds per task to conflicts and build fixes, counting the extra CI
cycle of a pull request sent back (2.1 at 16 agents on hot), so the calls
alone cost more in 7 of 9 cells.

### How the agents act on notes matters

Policy variants of gitlet, against plain git on the same backlogs:

| profile | agents | policy | rounds vs git (range) | conflicts | build breaks + CI fails | rework lines | wait + blocked agent-rounds |
|---|---|---|---|---|---|---|---|
| clustered | 8 | plain git | 1 | 2.7 | 1.0 | 65.0 | 0 |
| clustered | 8 | defer on notes and `who` (default) | 1.11 (1.05-1.21) | 2.3 | 0.3 | 28.3 | 23.7 |
| clustered | 8 | never defer | 1.01 (0.96-1.04) | 2.7 | 1.3 | 31.0 | 0 |
| hot | 8 | plain git | 1 | 5.3 | 2.7 | 90.7 | 0 |
| hot | 8 | defer on notes and `who` (default) | 1.19 (1.00-1.35) | 3.7 | 2.3 | 89.0 | 53.7 |
| hot | 8 | never defer | 1.06 (0.96-1.22) | 6.7 | 2.7 | 115.0 | 3.3 |
| hot | 8 | act on each note with probability ½ | 1.21 (1.04-1.33) | 4.0 | 2.7 | 102.7 | 65.0 |
| hot | 8 | abandon only on act duplicate notes | 1.28 (1.06-1.43) | 3.7 | 2.3 | 99.0 | 79.7 |
| spread | 16 | plain git | 1 | 3.3 | 0.7 | 159.3 | 0 |
| spread | 16 | defer on notes and `who` (default) | 0.99 (0.98-1.00) | 2.0 | 0.3 | 21.0 | 24.0 |
| spread | 16 | never defer | 0.95 (0.89-0.98) | 3.7 | 0.3 | 42.3 | 0.7 |
| clustered | 16 | plain git | 1 | 9.0 | 1.7 | 251.0 | 0 |
| clustered | 16 | defer on notes and `who` (default) | 1.07 (1.04-1.10) | 6.7 | 1.3 | 105.0 | 57.0 |
| clustered | 16 | never defer | 0.99 (0.98-1.00) | 8.3 | 1.0 | 117.7 | 0 |
| hot | 16 | plain git | 1 | 25.0 | 24.7 | 483.7 | 0 |
| hot | 16 | defer on notes and `who` (default) | 1.38 (1.15-1.59) | 21.3 | 14.0 | 388.3 | 376.3 |
| hot | 16 | defer on notes, not on `who` | 1.34 (1.09-1.50) | 20.7 | 12.7 | 399.3 | 264.7 |
| hot | 16 | never defer | 1.02 (0.96-1.08) | 26.0 | 20.3 | 453.3 | 2.0 |

"Never defer" ignores both the overlap notes and `who`; its few remaining
rounds are blocked ones, or ops yielded to the owner of a signature they
need after a `signature_clash`. "Probability ½" skips half the notes but
still defers on every `who` result, which caused 78% of its deferral, so it
does not halve the waiting. Deferring on notes but not on `who` hardly
changed the makespan at 16 agents on hot (1.34 against 1.38), though it cut
the waiting by about 30% (264 against 374 agent-rounds): about 60% of the
op-rounds `who` no longer put off were put off by `predicted_conflict` and
`body_overlap` notes instead. The cost is in deferring at all, whatever
triggers it.

Under the simulation's cost model, deferring bought on hot 3.0 fewer
conflicts and 26 fewer rework lines per run at 8 agents for 12% more
makespan, and 4.7 fewer conflicts, 6.3 fewer build breaks and 65 fewer rework
lines at 16 agents for 35% more. On clustered and spread it bought a third
of a conflict to 1.7 conflicts for 4% to 9% more. In that model a conflict or
build break is always resolved correctly, at a round plus a round per 20
lines, and no tests run; where conflicts are costlier or resolved wrongly,
deferring could pay.

Never deferring, gitlet still had less rework than plain git at 16 agents
(42 against 159, 118 against 251 and 453 against 484 lines), but that is
duplicate work dropped early by the ground-truth judge. Without it,
never-defer gitlet had about as much rework as git on spread (42 against
40), a little less on clustered (112 against 123) and more on hot (451
against 382). The duplicates are also the likeliest reason it beat plain git
at spread 16: on the backlog with nine duplicates, git spent 94 agent-rounds
on duplicate tasks before finding them already landed, and never-defer
gitlet finished in 42 rounds against git's 47; on the other two backlogs it
was one round faster.

The two policies at 16 agents on spread and clustered did not run under the
same load: default runs took 18 to 21 s of wall time a round and never-defer
runs 9 to 12 s, so gitlet's timeouts lasted about half as many rounds in the
default runs. The comparison there carries that difference. At 16 agents on
hot the two ran at about the same speed.

### Are the notes right?

The oracle merges every pair of tasks applied alone to the base, and builds
the clean merges that can break: a pair interacts if it conflicts, breaks
the build, or duplicates. These are the pairs that were in flight together in
the main gitlet runs.

| profile | agents | interacting pairs | conflict / break / duplicate | pairs warned | warned and interacting | recall | recall without duplicates | precision | notes about interacting pairs | duplicate pairs named, true / all |
|---|---|---|---|---|---|---|---|---|---|---|
| spread | 8 | 6 | 6 / 0 / 4 | 4 | 1 | 17% | 50% | 25% | 26% | 4 / 16 |
| spread | 16 | 30 | 27 / 2 / 15 | 18 | 4 | 13% | 27% | 22% | 28% | 15 / 63 |
| clustered | 8 | 16 | 13 / 3 / 4 | 16 | 7 | 44% | 58% | 44% | 27% | 4 / 28 |
| clustered | 16 | 59 | 52 / 7 / 16 | 48 | 15 | 25% | 35% | 31% | 20% | 16 / 144 |
| hot | 8 | 24 | 16 / 8 / 3 | 67 | 11 | 46% | 52% | 16% | 12% | 3 / 69 |
| hot | 16 | 187 | 117 / 70 / 13 | 443 | 100 | 53% | 57% | 23% | 25% | 13 / 237 |

Recall is the share of interacting pairs a warn or act note named; most
duplicate pairs also conflict, and duplicate notes are info, so recall
without duplicates is the fairer measure of the warnings. The counts at 8
agents are small. On average a warning came 1 to 12 rounds before the first
of the two tasks landed (a median of 0 to 8 rounds at 8 and 16 agents); 13
of the 138 warned interacting pairs were first warned in or after the round
the first of them landed. In the 27 main gitlet runs the agents read 3,251
notes (2,535 distinct ones; a note is delivered again when it changes): 34%
of the reads were info duplicate notes and 22% info body-overlap notes.

The `predicted_conflict` warnings run `git merge-tree` on the two branches,
yet 294 of the 394 pairs they named do not interact on the base (314 do not
conflict). In 292 of those 294, one of the two tasks conflicts on the base with a task that was
already on main when the note came, so the prediction most likely picks up a
conflict with main (one branch carries the landed task, the other does not)
rather than one with the peer.

### Cost per task

About 14 to 22 gitlet calls per task, two thirds of them `status`, and 0.1 to
9.6 notes (90 to 7,000 characters) per task, rising with contention. gitlet's
time per call rose with the number of agents, from 0.6 s at 4 agents to about
4 s at 16. These runs shared four CPUs with Go builds, so part of that is
contention, but the benchmark does not separate it from gitlet's own work per
peer. gitlet's call time costs no rounds: rounds advance together, and a slow
call only makes the round longer in wall time. At 16 agents gitlet rounds
took 8 to 21 s against plain git's 8 to 12; if that is gitlet's own work, it
is a cost at scale that the makespan does not show. The 0.3 s median in the
agent study is for two agents.

### What the simulation does not capture

- Its agents follow a fixed policy and never misread a note. They wait where
  the skill does not say to (on warn notes and `who` results) and never
  `discuss`. They judge every info duplicate note with ground truth, at no
  cost, which is the best case for gitlet, and they obey act duplicate notes
  without judging them.
- An agent with nothing runnable waits instead of taking another task, and a
  finished task refused for an unlanded contract waits instead of taking the
  `release` option gitlet offers. Both make gitlet look slower.
- `start` is given each task's exact file list, and `status` and reading
  notes cost nothing. Both help gitlet. File locks get the same file lists.
- Conflicts and build breaks are resolved by redoing the task's ops on the
  new main, at a round plus a round per 20 lines, and CI only builds, so
  semantic interactions are invisible to every mode and to the ground truth.
  A break the ops still cause on the new main cannot be fixed that way: on the
  hot backlog of seed 1, two tasks' new calls form an initialization cycle
  together, and one of them got stuck in every 16-agent run on it. Nothing varies that cost,
  the quarter round per coordination call, or the 25-round cap on deferring,
  and the verdict rests on all three.
- Ground truth is against the base; interactions that only appear on a later
  main are not counted.
- A gitlet round took 8 to 12 s of wall time at 8 agents and 8 to 21 s at
  16, depending on what else ran on the machine, against the six seconds
  gitlet's timeouts were scaled for, so thread and publish timeouts lasted
  fewer rounds than intended, by different amounts in different runs.
- Two of nine backlogs at 16 agents leave main failing `go vet` in every mode:
  one generated task's caller update also rewrote calls to `testing.T.Run` in
  test files, which only vet compiles. It is the same in every mode.
- In the rebase-whenever-main-moves runs, an agent that found during a sync
  that its task had already landed as a duplicate crashed in that round (15
  times). The task had already ended, so the results are unaffected; the
  harness now returns instead.
- Runs stop at 400 rounds; only file locks reached that.

## The agent study

Two Claude Code agents at once, on kvsvc, each scenario twice per mode. The
same task and rules in both modes. In gitlet mode the prompt adds gitlet's
skill file and replaces the landing steps: there is no MCP server, no client
hooks and no pull-request server, so the agents run `gitlet status` after
each commit and before landing, then `gitlet submit`, `gitlet update`, the
checks and a push to main themselves, then a final `gitlet status`. Part of
the overhead below is this setup.

| scenario | makespan s, git | makespan s, gitlet | gitlet / git | output tokens, git | output tokens, gitlet | gitlet / git |
|---|---|---|---|---|---|---|
| signature | 73 | 171 | 2.35 | 6,531 | 17,574 | 2.69 |
| samefunc | 78 | 118 | 1.52 | 6,894 | 11,646 | 1.69 |
| duplicate | 95 | 168 | 1.76 | 11,583 | 16,858 | 1.46 |
| independent | 54 | 84 | 1.55 | 4,438 | 7,259 | 1.64 |

Makespan runs until both agents had reported; to the last push to main it is
1.6 to 2.7 times as long with gitlet. In absolute terms gitlet added 30 s
(independent), 40 s (samefunc), 73 s (duplicate) and 99 s (signature) per
run on average (19 to 124 s in single runs, against the mean of the git
runs of the scenario), and 2.8k, 4.8k, 5.3k and 11.0k output tokens. Every run in both modes
landed both tasks on main with build, vet, tests and the hidden acceptance
tests passing.

- **signature** (a changes `store.New`'s signature, b adds a caller). With
  git, in both runs b landed first, a's rebase went cleanly but broke the
  build, and a fixed the call in one commit within seconds. With gitlet, b
  recorded a `need` and `use`d a's stub, and in both runs the stub gitlet
  built did not compile (see above). a found this by building it and
  republished its whole commit as the stub. b, whose code now called the new
  `New`, had to wait for a to land: in one run its `submit` was refused until
  then, in the other it waited without trying. So the landing order was the
  reverse of git's, with b idle for 27 and 55 s. Both b agents also got a
  `predicted_conflict` warning about the stub they had picked, and dismissed
  it.
- **samefunc** (both change `store.Put`). With gitlet, `who` before editing
  listed nobody for all four agents, and the warning came only once both had
  committed: to the later agent (to both, in one run). In one run it came 4 s
  before the first agent landed, and the thread started after that. The
  agents settled that whoever landed second would keep both checks and
  merge, and the second agent then resolved the conflict by hand. `gitlet
  update` refused to rebase into the conflict, so it was done with plain git,
  as the git agents did without talking. Every run in both modes had one
  conflicted rebase or merge.
- **duplicate** (the same task, worded differently). With git, in both runs
  one agent wrote and committed a full implementation (167 and 179 lines),
  found its push rejected, saw the other's work on main and threw its own
  away. With gitlet, in each run one agent (once the one that had started
  first) saw the other's task in `gitlet status` (once also its
  work-in-progress snapshot), wrote no code, and polled main until the other
  landed. No duplicate note was raised: the duplicate rule compares
  the intents, and the two wordings scored 0.31 and 0.32 against its 0.5
  threshold.
- **independent** (no overlap). Pure overhead: 1.55 times the time.

Rough edges the agents hit:

- `gitlet submit "title"` is a usage error (the title must be `--title`); 7
  of 8 gitlet runs hit it. The skill's examples are MCP-style. It gives the
  shell rule (required arguments first, the rest as flags) with one `need`
  example, but does not say which arguments are required.
- After landing by a direct push, the next `status` still listed the task as
  submitted, for all 14 landed tasks; for 13 that was the last `status` the
  agent ran. From the CLI gitlet records the
  landing only after that call has printed its result, it sees main move only
  on its own fetch, and it reuses a fetch less than 5 s old.
- `who` refuses a method named without its receiver (`store.go:Put` for
  `Store.Put`; in 6 of 8 runs). Before the other agent had committed or
  claimed anything, it listed nobody, on `Client.Fetch` in both duplicate runs
  and on `Store.Put` in both samefunc runs, while that agent had already
  started the same or an overlapping task. So the `who` the skill asks for
  before every shared edit never showed the overlap in those runs.
- 12 calls were `--help`.

A first pass of the study (16 runs and a pilot pair), with a different
signature scenario and earlier prompts, gave the same picture (gitlet 1.4 to
1.8 times as long per pair of runs, except one duplicate run at 0.91) and is
not counted here.

## What would make gitlet lose less, and what a win would take

Each change below removes a cost measured here; none is shown to make gitlet
win. In the simulation, gitlet's notes cut conflicts only through waiting.
Without it gitlet was within 6% of plain git on makespan, with as many
conflicts or more. With it, at 16 agents on hot, gitlet spent about 400
agent-rounds per run more than git on waiting and coordination, to save
about 90 in its tasks: about 45 because its agents dropped duplicate tasks
at once using ground truth, and about 45 from 14 fewer conflicts, build
breaks and CI failures. For the waiting to pay, each of those would have to
cost 25 to 30 agent-rounds instead of about 3. In the agent study, git's
worst interaction threw away one agent's 91 to 99 seconds of work, while
gitlet cost 55 agent-seconds even on the runs with no interaction, and 140 on
the duplicate runs themselves. A win needs interactions that cost plain git
far more than they did here (longer tasks, conflicts that are expensive or
easy to get wrong), and agents that use the notes for more than waiting.

1. **Make stubs build.** For a breaking change, gitlet's own stub cannot
   build while main has callers. Carry the owner's caller updates and the new
   declarations its body uses into the stub, so that it builds and can land
   on its own, and let a project configure a build command so gitlet can
   check a stub before publishing it. Republishing the owner's whole commit
   (`publish --commit`), as both owners did, makes the stub build but ties
   every dependent to the owner's landing: in both runs the dependent then
   waited 27 to 55 s.
2. **Make landing first cheap.** Today a task that used a contract can land
   before it only by hand: `release` the need, `git revert` the picked stub,
   and adapt to main's signature. gitlet could do the revert and check the
   branch against main itself. gitlet's own answer, landing the stub first as
   an interface-only pull request (`submit symbol=`), was not tested here:
   the skill does not mention it, the stubs did not build, and the sandbox
   had no pull-request server. Teach it in the skill once stubs build, and
   measure it.
3. **Keep showing what others are doing.** The list of every other agent's
   task in `status` is what stopped duplicate work with real agents, helped
   once by the other agent's work-in-progress snapshot. `start` lists only
   tasks above the duplicate rule's threshold and listed none; show the full
   list there too. The duplicate rule found every true duplicate in the
   simulation, but named 3 to 22 times as many false ones, nearly all at
   info; with real agents it raised nothing. Either make it precise enough to
   raise true duplicates at act severity, or drop it in favour of the task
   list. `who` also needs to see an agent that has started but not yet
   committed or claimed anything.
4. **Fix `predicted_conflict`.** It already runs `git merge-tree`, yet 294
   of the 394 pairs it named do not interact on the base, and in 292 of them
   one of the two conflicts with a task already on main. Predicting on both
   heads rebased onto current main, and reporting a conflict with main
   through `stale` or `update` rather than as a pair, would likely cut most
   of these. Say in the skill what to do with a warn overlap note (keep
   working; `discuss` when both must change the same lines). The real agents
   already kept working on warn notes; this would rule out the waiting the
   simulation's policy assumed.
5. **Need fewer round trips.** The extra tokens track the extra turns, not
   gitlet's output. `status` (78 calls) and `who` (50) are most of the 230
   gitlet commands the agents ran in the eight runs. gitlet already returns
   new notes in every verb's result (up to three in full) and prints new act
   notes after `git commit`; what the results leave out is the open notes in
   full and the other agents' tasks. About 50 of the 78 `status` calls were
   the ones this sandbox's prompt required (at the start, after each commit,
   after landing). Measure again without that instruction, and with MCP and
   hooks; in gitlet itself, the lever left is `who`, which the skill asks for
   before every shared edit and which showed nothing useful in these runs.
