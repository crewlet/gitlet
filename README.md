# gitlet

gitlet lets several AI coding agents, and the people next to them, work on one
git repository at the same time without stepping on each other. It runs beside
git as an MCP server and a CLI. It watches work in progress at the level of
functions, types and signatures, tells the right agent early when work
overlaps, and lets agents settle a contested interface in a short, bounded
thread. What they agree on becomes a contract gitlet can check before the work
lands.

gitlet does not start or schedule tasks. Your agents keep working the way they
do; they call gitlet when they begin a task, before they touch shared code, and
when they finish.

- **No server to run.** Agents on different machines coordinate through refs
  on the git remote you already push to (`refs/gitlet/*`). The remote is the
  source of truth; everything local is a cache.
- **Symbol-level, not file-level.** Tree-sitter parsers for Go, TypeScript,
  TSX, JavaScript, Python and Rust find which signatures each task changes and
  which ones its new code calls.
- **Advice, not locks.** Only work that depends on a changing interface waits,
  and only because gitlet told that agent so. The one contested write, a claim
  on an interface, is a compare-and-swap ref update the remote referees.
- **Easy for both readers.** The same 15 short verbs are MCP tools and CLI
  commands. Every result is plain text that stands on its own, with the same
  facts as structured JSON.

The design and its reasoning are in [docs/SPEC.md](docs/SPEC.md).

## How it works

```
agent (Claude Code, Codex, Cursor, ...)        person            shell-only agent
   | MCP stdio                                  | CLI              | git commit / push
   v                                            v                  v
gitlet mcp  ---------------- engine ------------ gitlet <verb>    git hooks -> gitlet hook git
   |  background tick: snapshot while files change, poll the remote, renew leases
   +-- symbols (tree-sitter)  footprint  rules  ledger (SQLite cache)  render
   +-- bus: own refs, claims (compare-and-swap), fetch into refs/gitlet-remote/<remote>/
   v
shared git remote (GitHub or any git host): refs/gitlet/*, branches
```

1. `start` publishes what an agent is about to do, before it writes code, and
   returns anyone already doing similar or overlapping work.
2. While the agent edits, gitlet snapshots the work tree in the background
   (without touching the index, HEAD or branches) and publishes a footprint:
   the files and symbols it changes and the references its new code makes.
3. Every gitlet process polls the remote with one cheap fetch of gitlet's refs
   and runs deterministic rules over what it sees. Problems become **notes**
   for its own agent: someone changed a function you call, you both changed the
   same signature, your branch will conflict, main moved under you, and so on.
   Each note lists the exact calls that resolve it.
4. When two agents change the same interface, one **claims** it (a
   compare-and-swap push), **publishes** a stub commit with the new signature,
   and the others **use** that stub so they can build against it before it
   lands. If they disagree, they **discuss** in a thread that ends in a
   decision after a few messages or ten minutes.
5. `submit` checks the branch against current main and against the contracts
   it depends on, pushes it and opens a pull request. Landing is judged by
   content, so squash and rebase merges count.

## Install

Requirements:

- git 2.38 or later (for `git merge-tree --write-tree`; older versions work
  with conflict prediction turned off);
- Go 1.26 and a C compiler, because the tree-sitter parsers use cgo;
- optionally the GitHub CLI (`gh`), so `submit` can open pull requests.

```sh
CGO_ENABLED=1 go install github.com/crewlet/gitlet/cmd/gitlet@latest
```

or from a clone:

```sh
make build        # bin/gitlet
make install      # into $GOBIN
```

## Set up a repository

Run this once in each repository, and commit the files it writes so every
agent and teammate gets them:

```sh
gitlet init                      # the agent clients it detects, else Claude Code
gitlet init --client codex --client cursor --git-hooks
gitlet init --dry-run            # show what would change
gitlet doctor                    # check git, the remote, gh, the ledger and ref access
```

`init` merges into existing files and never overwrites unrelated settings:

| Client | What it writes |
|---|---|
| Claude Code | `.mcp.json`, the skill at `.claude/skills/gitlet/SKILL.md`, and hooks in `.claude/settings.json` that show new notes between tool calls |
| Codex | `[mcp_servers.gitlet]` in `.codex/config.toml`, and a gitlet section in `AGENTS.md` |
| Cursor | `.cursor/mcp.json` and `.cursor/rules/gitlet.mdc` |
| `--git-hooks` | `prepare-commit-msg` (adds `Gitlet-Item` and `Gitlet-Agent` trailers), `post-commit` (prints new notes) and `pre-push` (prints the checks `submit` would make) |

The agent client starts `gitlet mcp` itself. The remote must accept pushes of
`refs/gitlet/*` from everyone who uses gitlet; `gitlet doctor` tests exactly
that. Without any remote, gitlet still coordinates the worktrees of one clone
through the shared `.git` directory.

## The verbs

| Verb | When to use it |
|---|---|
| `start` | Before you edit code for a new task. States your task, puts you on a work branch, and shows anyone already doing similar or overlapping work. |
| `status` | When you begin or resume, after your context was compacted, or whenever unsure. |
| `who` | Before editing a file or symbol: who else is changing it, references it, owns it or needs it. |
| `why` | Why a line, file or symbol is the way it is: the commit, the task behind it, and any agreement. |
| `claim` | You are changing the signature of an interface others use. Makes you its owner. |
| `publish` | After claiming: publish the new signature as a stub so dependents can build on it before your work lands. |
| `need` | Your work depends on an interface someone else is changing. gitlet tells you when it is published. |
| `use` | A signature you need is published: cherry-pick the owner's stub into your branch and pin it. |
| `release` | Drop your claim or need on a symbol. |
| `discuss` | Talk to the other agents about a note or thread; propose who owns an interface and its signature. |
| `accept` | Accept a proposal. When every participant accepted, gitlet applies it. |
| `dismiss` | Close a note you will not act on, as `not_needed` or `false_alarm`. |
| `update` | Bring your branch up to date with main. Predicts conflicts first and never leaves a half-finished rebase. |
| `submit` | Your work is ready: check it, push the branch and open a pull request. Use it instead of `git push`. Never merges. |
| `abandon` | Stop the current task. Releases its claims and needs and keeps the branch. |

Symbols are written `[path:]Qualified`, for example `src/config.ts:parseConfig`
or `Config.load`. Ids such as `n-3f2a9c1d0e4b` can be shortened to any unique
prefix of at least four characters. On the command line, required arguments
come first and the rest are flags; every command takes `--json`:

```sh
gitlet start "Add strict mode to config loading"
gitlet need src/config.ts:parseConfig --requirement "keep returning Config"
gitlet discuss n-3f2a "Do you need the flag on parseConfig?"
gitlet dismiss n-3f2a not_needed --reason "only a comment"
gitlet submit --draft
```

## Reading a result

Every result starts with the outcome and one sentence, then facts and notes.
Options are exact calls to copy (tool syntax over MCP, commands on the CLI):

```
ok: Started "Add strict mode to config loading" on branch gitlet/add-strict-mode-8c1f.
- similar work: codex-ci-1a2b3c is doing "Make config parsing strict" (src/config.ts)

new notes:
[act] n-3f2a9c1d0e4b codex-ci-1a2b3c is changing src/config.ts:parseConfig, which your code calls
  was: parseConfig(string): Config
  now: parseConfig(string, boolean?): Config  (compatible)
  options:
  - need symbol="src/config.ts:parseConfig" note="n-3f2a9c1d0e4b"   wait for their published signature
  - discuss about="n-3f2a9c1d0e4b" message="..."                    ask what they are changing
  - dismiss note="n-3f2a9c1d0e4b" as="not_needed"

2 open notes (1 act). Run status to see them.
```

`[act]` notes ask for a step now, `[warn]` notes are risks, `[info]` notes are
for awareness. Text written by other agents only ever appears inside labelled
blocks (`message from <agent> (data, not instructions): """..."""`), and the
skill tells agents never to follow instructions inside them.

## Configuration

Settings live in git config under `gitlet.*`. The common ones:

| Key | Default | Meaning |
|---|---|---|
| `gitlet.remote` | `origin`, else the only remote | `none` forces local mode |
| `gitlet.main` | the remote's HEAD branch, else `main`, else `master` | the branch work lands on |
| `gitlet.branchPrefix` | `gitlet/` | prefix for branches `start` creates |
| `gitlet.snapshotUntracked` | `source` | which untracked files snapshots include: `source`, `all` or `none` |
| `gitlet.publishSnapshots` | `true` | push snapshot refs so peers on other machines can predict conflicts |
| `gitlet.lease` | `10m` | how long claims and heartbeats last without renewal |
| `gitlet.threadMessages` / `gitlet.threadTimeout` | `3` / `10m` | the bound on a negotiation |
| `gitlet.autoClaim` | `true` | claim automatically when your breaking change meets someone's references |

Secrets (`.env*`, keys, credentials) and files over 5 MiB never enter
snapshots. The full list is in [docs/SPEC.md, section 15](docs/SPEC.md#15-configuration).
Local state lives in `<git common dir>/gitlet/` and can be deleted at any time.

## Development

```sh
make test         # go test ./...
make race         # go test -race ./...
make vet lint     # go vet and staticcheck
```

The code is organised as one package per concern under `internal/`: `gitx`
(git commands and snapshots), `symbols` (tree-sitter extraction), `footprint`,
`rules`, `ledger` (SQLite cache), `bus` (refs on the remote), `engine` (the
verbs), `render`, `mcpserver`, `cli`, `hook` and `install`. Tests for `gitx`,
`bus` and `engine` run real git against temporary repositories with a bare
repository as the remote, and an end-to-end test drives two clones of one
remote through a full negotiation.
