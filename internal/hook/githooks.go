package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// maxConflictFiles caps the conflicted paths pre-push lists.
const maxConflictFiles = 5

// git runs a git hook (docs/SPEC.md, section 14.2). Git runs hooks at the
// top of the work tree, so the working directory names the repository.
func (r *runner) git(ctx context.Context, args []string) {
	if len(args) == 0 {
		r.usage()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.gitBudget)
	defer cancel()
	var err error
	switch args[0] {
	case "prepare-commit-msg":
		if len(args) < 2 || args[1] == "" {
			r.usage()
			return
		}
		err = r.prepareCommitMsg(ctx, args[1:])
	case "post-commit":
		err = r.postCommit(ctx)
	case "pre-push":
		err = r.prePush(ctx, args[1:])
	default:
		r.usage()
		return
	}
	r.report(err)
}

// prepareCommitMsg adds Gitlet-Item and Gitlet-Agent trailers to the message
// file when the commit belongs to an active item of the worktree's agent
// (docs/SPEC.md, section 12.5; see pickItem). args are git's: the file, then
// the message source and a commit.
func (r *runner) prepareCommitMsg(ctx context.Context, args []string) error {
	file, source := args[0], ""
	if len(args) > 1 {
		source = args[1]
	}
	switch source {
	case "merge", "squash":
		// They combine other work, which carries its own trailers.
		return nil
	case "template":
		// The trailers would defeat git's "you did not edit the message"
		// check, so an untouched template would commit instead of aborting.
		return nil
	}
	dir, err := r.getwd()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	s, err := r.openSession(ctx, dir)
	if err != nil || s == nil {
		return err
	}
	defer s.close()
	if s.agent == "" {
		return nil
	}
	// A rebase or cherry-pick replays existing commits (update's rebase, use's
	// cherry-picked stubs), which keep the provenance they already have.
	switch op, err := s.repo.Operation(ctx); {
	case err != nil:
		return err
	case op == "rebase" || op == "cherry-pick" || op == "merge":
		return nil
	}
	items, branch, err := agentItems(ctx, s.repo, s.agent)
	item := pickItem(items, branch)
	if err != nil || item == nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	msg, changed := addTrailers(string(data), commentPrefix(ctx, s.repo), string(item.ID), string(s.agent))
	if !changed {
		return nil
	}
	return writeFileAtomic(file, []byte(msg))
}

// commentPrefix returns what starts a comment line in commit messages:
// core.commentChar, or "#" when it is unset or "auto".
func commentPrefix(ctx context.Context, repo *gitx.Repo) string {
	c, err := repo.ConfigGet(ctx, "core.commentChar")
	if err != nil || c == "" || c == "auto" {
		return "#"
	}
	return c
}

// postCommit prints the agent's new act notes to stderr, where whoever ran
// git commit sees them, and marks shown the notes it printed in full. The
// rest, and every other new note, stay new for the next result or hook.
// Commits git replays print nothing (see replayed).
func (r *runner) postCommit(ctx context.Context) error {
	dir, err := r.getwd()
	if err != nil {
		return err
	}
	s, err := r.openSession(ctx, dir)
	if err != nil || s == nil {
		return err
	}
	defer s.close()
	if s.agent == "" || s.ledger.ReadOnly() {
		return nil
	}
	acts, err := s.newNotes(ctx, model.SeverityAct)
	if err != nil || len(acts) == 0 {
		return err
	}
	if replay, err := replayed(ctx, s.repo); err != nil || replay {
		return err
	}
	text := strings.TrimRight(render.Notes(notesHeading, acts, render.CLI), "\n")
	if text == "" {
		return nil
	}
	fmt.Fprintln(r.stderr, text)
	s.markShown(ctx, shownInFull(acts), r.now())
	return nil
}

// maxReflogTail is how much of the end of HEAD's reflog replayed reads to
// find its last entry.
const maxReflogTail = 8 << 10

// replayed reports whether the commit that ran post-commit was replayed by
// a rebase, cherry-pick or revert rather than made with git commit.
// gitlet's use and update replay commits with hooks enabled and swallow
// their output on success, so notes printed there would reach nobody and
// would no longer be new for the verb's result. A revert leaves no state
// behind while its hooks run; the entry it wrote to HEAD's reflog just
// before tells it apart. Without that reflog (it is off, or kept in a
// reftable) a revert passes for a commit.
func replayed(ctx context.Context, repo *gitx.Repo) (bool, error) {
	switch op, err := repo.Operation(ctx); {
	case err != nil:
		return false, err
	case op == "rebase" || op == "cherry-pick" || op == "revert":
		return true, nil
	}
	f, err := os.Open(filepath.Join(repo.GitDir, "logs", "HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	off := max(fi.Size()-maxReflogTail, 0)
	tail := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	// An entry is "<old> <new> <ident> <time> <zone>\t<action>: <subject>",
	// such as "commit (amend): ...", "cherry-pick: ..." or "rebase (pick): ...".
	last := strings.TrimRight(string(tail), "\n")
	last = last[strings.LastIndexByte(last, '\n')+1:]
	_, msg, _ := strings.Cut(last, "\t")
	action, _, _ := strings.Cut(msg, ":")
	return action == "cherry-pick" || action == "revert" || strings.Contains(action, "rebase"), nil
}

// prePush prints, when the current branch belongs to an active item, the
// submit checks a hook can make offline (docs/SPEC.md, section 12.4) and
// the reminder that gitlet submit opens the pull request. It never blocks the push: every
// check is best effort, and failed checks are left out.
func (r *runner) prePush(ctx context.Context, args []string) error {
	var remote string
	if len(args) > 0 {
		remote = args[0]
	}
	dir, err := r.getwd()
	if err != nil {
		return err
	}
	s, err := r.openSession(ctx, dir)
	if err != nil || s == nil {
		return err
	}
	defer s.close()
	items, branch, err := agentItems(ctx, s.repo, s.agent)
	item := pickItem(items, branch)
	if err != nil || item == nil {
		return err
	}

	var checks []string
	var errs []error
	if st, err := s.repo.Status(ctx); err != nil {
		errs = append(errs, err)
	} else if len(st) > 0 {
		checks = append(checks, fmt.Sprintf("%s not in this push; submit refuses until they are committed.",
			plural(len(st), "uncommitted change is", "uncommitted changes are")))
	}
	if main, conflicts, err := mainConflicts(ctx, s.repo, remote); err != nil {
		errs = append(errs, err)
	} else if len(conflicts) > 0 {
		checks = append(checks, fmt.Sprintf("merging into %s as last fetched conflicts in %s; run gitlet update first.",
			main, listPaths(conflicts, maxConflictFiles)))
	}
	if _, act, err := s.ledger.Counts(ctx, s.agent); err != nil {
		errs = append(errs, err)
	} else if act > 0 {
		checks = append(checks, fmt.Sprintf("%s: run gitlet status and act on them.", plural(act, "open act note", "open act notes")))
	}
	fmt.Fprint(r.stderr, prePushReport(item, checks))
	return errors.Join(errs...)
}

// mainConflicts predicts whether HEAD merges cleanly into remote's main as
// last fetched, without touching the network. main names the branch it
// used ("<remote>/<branch>"); both results are empty when no fetched main
// is known, for example when remote is a URL.
func mainConflicts(ctx context.Context, repo *gitx.Repo, remote string) (main string, conflicts []string, err error) {
	if remote == "" {
		return "", nil, nil
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil || !slices.Contains(remotes, remote) {
		return "", nil, err
	}
	branch, err := repo.ConfigGet(ctx, "gitlet.main")
	if err != nil {
		return "", nil, err
	}
	if branch == "" {
		if branch, err = repo.DefaultBranch(ctx, remote); err != nil || branch == "" {
			return "", nil, err
		}
	}
	// gitlet's own copy of main is usually fresher than the user's
	// remote-tracking branch (docs/SPEC.md, section 6.2).
	var oid string
	for _, ref := range []string{"refs/gitlet-main/" + remote, "refs/remotes/" + remote + "/" + branch} {
		oid, err = repo.RevParse(ctx, ref)
		if err == nil {
			break
		}
		if !errors.Is(err, gitx.ErrNotFound) {
			return "", nil, err
		}
	}
	if oid == "" {
		return "", nil, nil
	}
	res, err := repo.MergeTree(ctx, "HEAD", oid)
	if err != nil || res.Clean {
		return "", nil, err
	}
	return remote + "/" + branch, res.Conflicts, nil
}

// prePushReport words what pre-push prints.
func prePushReport(item *model.WorkItem, checks []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gitlet: work item %s %q is active on this branch.\n", item.ID, clip(item.Intent, 80))
	for _, c := range checks {
		b.WriteString("- " + c + "\n")
	}
	b.WriteString("gitlet submit runs every check against fresh main, pushes the branch and opens the pull request; use it instead of git push.\n")
	return b.String()
}

// plural returns "<n> <one>" or "<n> <many>".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// listPaths joins up to max paths with ", " and says how many more there are.
func listPaths(paths []string, max int) string {
	if len(paths) <= max {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:max], ", "), len(paths)-max)
}
