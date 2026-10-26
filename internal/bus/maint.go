package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/gitlet/internal/gitx"
)

func (b *Bus) refreshMain(ctx context.Context) (string, error) {
	if b.remoteName == "" {
		oid, err := b.repo.RevParse(ctx, b.mainRef())
		if err != nil {
			return "", fmt.Errorf("bus: refresh main: %w", err)
		}
		return oid, nil
	}
	src := "refs/heads/" + b.mainBranch
	err := b.withFetchLock(ctx, func() error {
		return b.repo.Fetch(ctx, gitx.FetchOptions{
			Remote: b.remoteName,
			Refspecs: []string{
				"+" + src + ":refs/remotes/" + b.remoteName + "/" + b.mainBranch,
				"+" + src + ":" + b.mainRef(),
			},
			Metadata: true,
		})
	})
	if isMissingRemoteRef(err) {
		return "", fmt.Errorf("bus: refresh main: %s has no branch %s: %w", b.remoteName, b.mainBranch, gitx.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("bus: refresh main: %w", err)
	}
	oid, err := b.repo.RevParse(ctx, b.mainRef())
	if err != nil {
		return "", fmt.Errorf("bus: refresh main: %w", err)
	}
	return oid, nil
}

func (b *Bus) pushBranch(ctx context.Context, commit, branch, oldOID string) (gitx.PushResult, error) {
	switch {
	case b.remoteName == "":
		return gitx.PushResult{}, fmt.Errorf("bus: push branch: %w", errNoRemote)
	case branch == "" || !validBranch(branch):
		return gitx.PushResult{}, fmt.Errorf("bus: push branch: invalid branch %q", branch)
	case !validOID(commit):
		return gitx.PushResult{}, fmt.Errorf("bus: push branch: invalid commit %q", commit)
	case !validOptOID(oldOID):
		return gitx.PushResult{}, fmt.Errorf("bus: push branch: invalid old oid %q", oldOID)
	}
	dst := "refs/heads/" + branch
	// Not a metadata push: the user's pre-push hooks run (docs/SPEC.md, 6.1).
	push := func(u gitx.PushUpdate) (gitx.PushResult, gitx.PushReport, error) {
		rep, err := b.repo.Push(ctx, gitx.PushOptions{Remote: b.remoteName, Updates: []gitx.PushUpdate{u}})
		if err != nil {
			return gitx.PushResult{}, rep, fmt.Errorf("bus: push %s: %w", dst, err)
		}
		return resultFor(rep, dst), rep, nil
	}
	r, rep, err := push(gitx.PushUpdate{Src: commit, Dst: dst, Expect: gitx.ExpectFastForward})
	if err != nil {
		return r, err
	}
	if !accepted(r) && r.Kind == gitx.RejectNonFF && oldOID != "" {
		// Not a fast-forward: overwrite only what this clone pushed last.
		if r, rep, err = push(gitx.PushUpdate{Src: commit, Dst: dst, Expect: gitx.ExpectOID, OldOID: oldOID}); err != nil {
			return r, err
		}
	}
	if !accepted(r) {
		return r, fmt.Errorf("bus: %w", rejected(dst, r, rep))
	}
	return r, nil
}

func (b *Bus) reap(ctx context.Context, refs map[string]string) error {
	ups := make([]leaseUpdate, 0, len(refs))
	for _, ref := range sortedKeys(refs) {
		if !strings.HasPrefix(ref, Prefix) || !validOID(refs[ref]) {
			return fmt.Errorf("bus: reap: invalid ref %q at %q", ref, refs[ref])
		}
		ups = append(ups, leaseUpdate{ref: ref, old: refs[ref]})
	}
	if len(ups) == 0 {
		return nil
	}
	var errs []error
	if b.remoteName == "" {
		for _, u := range ups {
			// A ref that changed belongs to an agent that came back.
			if conflict, err := b.swapLocal(ctx, u.ref, "", u.old); err != nil && !conflict {
				errs = append(errs, fmt.Errorf("delete %s: %w", u.ref, err))
			}
		}
	} else {
		if err := b.writable(); err != nil {
			return fmt.Errorf("bus: reap: %w", err)
		}
		res, rep, err := b.pushLeased(ctx, ups)
		if err != nil {
			return fmt.Errorf("bus: reap: %w", err)
		}
		for i, u := range ups {
			switch r := res[i]; {
			case accepted(r):
				_ = b.recordPushed(ctx, u.ref, "", false)
			case r.Kind == gitx.RejectStale:
				// The agent came back, or someone else reaped it first.
			default:
				errs = append(errs, rejected(u.ref, r, rep))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("bus: reap: %w", err)
	}
	return nil
}

func (b *Bus) probe(ctx context.Context) (ProbeResult, error) {
	if b.remoteName == "" {
		return ProbeResult{Create: true, NonFastForward: true, Delete: true, Detail: "no remote: gitlet works on local refs"}, nil
	}
	ref := ProbePrefix + string(b.me)
	first, err := b.probeCommit(ctx, "create")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("bus: probe: %w", err)
	}
	second, err := b.probeCommit(ctx, "rewrite")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("bus: probe: %w", err)
	}
	var res ProbeResult
	var notes []string
	// step pushes one probe update and notes the outcome. It reports whether
	// the remote took it, and returns an error only for transport failures.
	step := func(name string, u leaseUpdate) (ok, stale bool, err error) {
		r, rep, err := b.pushLeased(ctx, []leaseUpdate{u})
		switch {
		case errors.Is(err, ErrObserver):
			notes = append(notes, name+": permission denied")
			return false, false, nil
		case err != nil:
			return false, false, fmt.Errorf("bus: probe %s: %w", name, err)
		case accepted(r[0]):
			notes = append(notes, name+": ok")
			return true, false, nil
		}
		note := name + ": " + rejected(ref, r[0], rep).Reason
		if len(rep.Remote) > 0 {
			note += " (" + strings.Join(rep.Remote, " / ") + ")"
		}
		notes = append(notes, note)
		return false, r[0].Kind == gitx.RejectStale, nil
	}
	ok, stale, err := step("create", leaseUpdate{ref: ref, oid: first})
	if err == nil && stale {
		// A probe ref left behind by an interrupted probe: remove it and try again.
		var old string
		if old, err = b.fetchExact(ctx, ref); err == nil && old != "" {
			_, _, err = step("remove leftover", leaseUpdate{ref: ref, old: old})
		}
		if err == nil {
			ok, _, err = step("create", leaseUpdate{ref: ref, oid: first})
		}
	}
	if err != nil {
		return res, err
	}
	if !ok {
		res.Detail = strings.Join(notes, "; ")
		return res, nil
	}
	res.Create = true
	b.setObserver(false)
	cur := first
	if res.NonFastForward, _, err = step("non-fast-forward", leaseUpdate{ref: ref, oid: second, old: first}); err != nil {
		return res, err
	}
	if res.NonFastForward {
		cur = second
	}
	if res.Delete, _, err = step("delete", leaseUpdate{ref: ref, old: cur}); err != nil {
		return res, err
	}
	res.Detail = strings.Join(notes, "; ")
	return res, nil
}

// probeCommit writes a small parentless commit for the probe ref.
func (b *Bus) probeCommit(ctx context.Context, step string) (string, error) {
	data, err := json.Marshal(map[string]any{"format": 1, "agent": b.me, "step": step, "at": b.now().UTC()})
	if err != nil {
		return "", err
	}
	return b.commitDoc(ctx, probeFile, data, probeMessage, "")
}
