package gitx

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func (r *Repo) mergeTree(ctx context.Context, ours, theirs string) (MergeResult, error) {
	if err := errors.Join(checkRev(ours), checkRev(theirs)); err != nil {
		return MergeResult{}, err
	}
	out, err := r.exec(ctx, cmdOpts{}, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	fields := splitNUL(out)
	switch code := exitCode(err); {
	case err == nil && len(fields) > 0:
		return MergeResult{Clean: true, Tree: fields[0]}, nil
	case code == 1 && len(fields) > 0:
		// The tree (with conflict markers) comes first, then each conflicted path.
		conflicts := slices.Clone(fields[1:])
		slices.Sort(conflicts)
		return MergeResult{Tree: fields[0], Conflicts: slices.Compact(conflicts)}, nil
	case err == nil:
		return MergeResult{}, fmt.Errorf("gitx: merge-tree printed no tree")
	}
	return MergeResult{}, mapNotFound(err, ours+" or "+theirs)
}

func (r *Repo) switchBranch(ctx context.Context, branch string) error {
	ok, err := r.branchExists(ctx, branch)
	if err != nil {
		return err
	}
	if !ok {
		return notFound("branch " + branch)
	}
	_, err = r.exec(ctx, worktreeOpts(nil), "switch", "--no-guess", branch)
	return err
}

func (r *Repo) createBranch(ctx context.Context, branch, startPoint string) error {
	if err := checkName("branch", branch); err != nil {
		return err
	}
	// --no-track: gitlet pushes branches itself, and an upstream of main
	// would make a plain git push target main.
	args := []string{"switch", "--no-track", "-c", branch}
	if startPoint != "" {
		if err := checkRev(startPoint); err != nil {
			return err
		}
		args = append(args, startPoint)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gitx: switch: %w", err)
	}
	_, err := r.exec(ctx, worktreeOpts(nil), args...)
	return err
}

// worktreeOpts runs a command that changes the work tree: detached from
// the caller's cancellation, with its own timeout.
func worktreeOpts(env []string) cmdOpts {
	return cmdOpts{env: env, timeout: timeoutWorktree, detached: true}
}

func (r *Repo) rebase(ctx context.Context, opt RebaseOptions) (ApplyResult, error) {
	var args []string
	// Commit messages never open an editor.
	env := []string{"GIT_EDITOR=true"}
	interactive := opt.SequenceEditor != ""
	if interactive {
		// The todo list keeps full command names for the editor to rewrite.
		args = append(args, "-c", "rebase.abbreviateCommands=false")
		env = append(env, "GIT_SEQUENCE_EDITOR="+opt.SequenceEditor)
	}
	// --empty=drop: -i would otherwise stop at commits that became empty,
	// such as a stub that landed on main in a slightly different form.
	args = append(args, "rebase", "--empty=drop")
	if interactive {
		args = append(args, "-i")
	}
	if opt.Autostash {
		args = append(args, "--autostash")
	} else {
		args = append(args, "--no-autostash")
	}
	if opt.Upstream != "" {
		if err := checkRev(opt.Upstream); err != nil {
			return ApplyResult{}, err
		}
		args = append(args, opt.Upstream)
	}
	return r.apply(ctx, "rebase", args, env)
}

func (r *Repo) cherryPick(ctx context.Context, commit string) (ApplyResult, error) {
	if err := checkRev(commit); err != nil {
		return ApplyResult{}, err
	}
	return r.apply(ctx, "cherry-pick", []string{"cherry-pick", "-x", commit}, []string{"GIT_EDITOR=true"})
}

func (r *Repo) revert(ctx context.Context, commit string) (ApplyResult, error) {
	if err := checkRev(commit); err != nil {
		return ApplyResult{}, err
	}
	return r.apply(ctx, "revert", []string{"revert", "--no-edit", commit}, []string{"GIT_EDITOR=true"})
}

// apply runs a rebase, cherry-pick or revert to the end. If git stops on the
// way, apply collects the conflicted paths and aborts, so the repository is
// never left mid-operation.
func (r *Repo) apply(ctx context.Context, op string, args, env []string) (ApplyResult, error) {
	switch cur, err := r.operation(ctx); {
	case err != nil:
		return ApplyResult{}, err
	case cur != "" && cur != "bisect":
		return ApplyResult{}, fmt.Errorf("gitx: %s: %s in progress: %w", op, cur, ErrBusy)
	}

	_, runErr := r.exec(ctx, worktreeOpts(env), args...)

	// From here on the caller's cancellation no longer counts.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeoutCleanup)
	defer cancel()
	if runErr == nil {
		head, err := r.revParse(cctx, "HEAD")
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{OK: true, Head: head}, nil
	}

	cur, err := r.operation(cctx)
	if err != nil {
		return ApplyResult{}, errors.Join(runErr, err)
	}
	if cur == "" || cur == "bisect" {
		return ApplyResult{}, runErr // refused before it started; nothing to undo
	}
	conflicts, conflictErr := r.unmergedPaths(cctx)
	if _, err := r.exec(cctx, worktreeOpts(nil), cur, "--abort"); err != nil {
		return ApplyResult{}, fmt.Errorf("gitx: %s stopped and could not be aborted: %w", op, errors.Join(runErr, err))
	}
	switch {
	case conflictErr != nil:
		return ApplyResult{}, fmt.Errorf("gitx: %s stopped (aborted): %w", op, errors.Join(runErr, conflictErr))
	case len(conflicts) == 0 || errors.Is(runErr, context.DeadlineExceeded):
		return ApplyResult{}, fmt.Errorf("gitx: %s stopped without conflicts (aborted): %w", op, runErr)
	}
	return ApplyResult{Conflicts: conflicts}, nil
}

// unmergedPaths lists the paths with conflicts in the index, sorted.
func (r *Repo) unmergedPaths(ctx context.Context) ([]string, error) {
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "diff", "--name-only", "--diff-filter=U", "-z", "--no-ext-diff", "--no-relative")
	if err != nil {
		return nil, err
	}
	paths := splitNUL(out)
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
