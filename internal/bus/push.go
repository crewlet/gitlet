package bus

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/flock"
	"github.com/crewlet/gitlet/internal/gitx"
)

// leaseUpdate is one ref update pushed with a lease (docs/SPEC.md, section 6.1).
type leaseUpdate struct {
	ref string // canonical ref
	oid string // new value; "" deletes the ref
	old string // value the remote must have; "" means it must not exist
}

func (u leaseUpdate) pushUpdate() gitx.PushUpdate {
	pu := gitx.PushUpdate{Src: u.oid, Dst: u.ref, Expect: gitx.ExpectAbsent}
	if u.old != "" {
		pu.Expect, pu.OldOID = gitx.ExpectOID, u.old
	}
	return pu
}

// accepted reports whether the remote took the update or already had it.
func accepted(r gitx.PushResult) bool { return r.OK || r.UpToDate }

// resultFor finds the result for dst in a push report. A ref git did not
// report on counts as rejected.
func resultFor(rep gitx.PushReport, dst string) gitx.PushResult {
	for _, r := range rep.Results {
		if r.Dst == dst {
			return r
		}
	}
	return gitx.PushResult{Dst: dst, Kind: gitx.RejectOther, Reason: "no result reported for the ref"}
}

func rejected(ref string, r gitx.PushResult, rep gitx.PushReport) *RejectedError {
	reason := r.Reason
	if reason == "" {
		reason = string(r.Kind)
	}
	return &RejectedError{Ref: ref, Kind: r.Kind, Reason: reason, Remote: rep.Remote}
}

// writable returns ErrObserver while the bus is read-only. Once observerRetry
// has passed since the last refusal, the next push is let through as the
// re-probe.
func (b *Bus) writable() error {
	b.obsMu.Lock()
	defer b.obsMu.Unlock()
	if !b.observerSince.IsZero() && b.now().Sub(b.observerSince) < observerRetry {
		return ErrObserver
	}
	return nil
}

func (b *Bus) setObserver(on bool) {
	b.obsMu.Lock()
	defer b.obsMu.Unlock()
	if on {
		b.observerSince = b.now()
	} else {
		b.observerSince = time.Time{}
	}
}

// pushLeased pushes updates in one metadata push (a lease per ref, never
// atomic) and returns each update's result in order. Transport failures are
// errors; a permission failure also switches the bus to observer mode.
func (b *Bus) pushLeased(ctx context.Context, ups []leaseUpdate) ([]gitx.PushResult, gitx.PushReport, error) {
	pus := make([]gitx.PushUpdate, len(ups))
	for i, u := range ups {
		pus[i] = u.pushUpdate()
	}
	rep, err := b.repo.Push(ctx, gitx.PushOptions{Remote: b.remoteName, Updates: pus, Metadata: true})
	if err != nil {
		return nil, rep, b.pushFailed(err)
	}
	res := make([]gitx.PushResult, len(ups))
	for i, u := range ups {
		res[i] = resultFor(rep, u.ref)
	}
	return res, rep, nil
}

// pushFailed maps a failed push invocation: permission errors switch the
// bus to observer mode and match ErrObserver; anything else is returned as is.
func (b *Bus) pushFailed(err error) error {
	if b.isPermission(err) {
		b.setObserver(true)
		return fmt.Errorf("%w: %w", ErrObserver, err)
	}
	return err
}

// recordPushed brings local bookkeeping in line with an accepted push: the
// tracking ref follows the remote, and for own refs the push log remembers
// the oid as the next lease. Tracking refs are a cache the next fetch also
// corrects, so failing to update one is not an error.
func (b *Bus) recordPushed(ctx context.Context, ref, oid string, own bool) error {
	b.setObserver(false)
	tr := b.tracking(ref)
	if tr != ref {
		if oid == "" {
			_ = b.repo.DeleteRef(ctx, tr, "")
		} else {
			_ = b.repo.UpdateRef(ctx, tr, oid, "")
		}
	}
	if !own {
		return nil
	}
	delete(b.refused, ref)
	if err := b.pushLog.SetLastPushed(ctx, ref, oid); err != nil {
		return fmt.Errorf("record push of %s: %w", ref, err)
	}
	return nil
}

// pushOwn pushes this agent's own refs in one push. Each update carries a
// lease on its old value (the oid last pushed; "" for create-only). A ref
// whose lease is stale is adopted: its remote value is read and the update
// retried once with a lease on it; a second stale lease means another
// process writes as this agent (ErrIdentityCollision). Any other rejection
// is remembered in b.refused. It returns one error per update, and an error
// for the push as a whole (transport, observer). Callers hold b.writeMu.
func (b *Bus) pushOwn(ctx context.Context, ups []leaseUpdate) ([]error, error) {
	errs := make([]error, len(ups))
	if err := b.writable(); err != nil {
		return errs, err
	}
	res, rep, err := b.pushLeased(ctx, ups)
	if err != nil {
		return errs, err
	}
	var retry []leaseUpdate
	var retryAt []int
	for i, u := range ups {
		switch r := res[i]; {
		case accepted(r):
			errs[i] = b.recordPushed(ctx, u.ref, u.oid, true)
		case r.Kind == gitx.RejectStale:
			remote, err := b.fetchExact(ctx, u.ref)
			if err != nil {
				return errs, fmt.Errorf("read remote %s: %w", u.ref, err)
			}
			if remote == u.oid {
				// The remote already has what we wanted, for example from a
				// push whose answer was lost.
				errs[i] = b.recordPushed(ctx, u.ref, u.oid, true)
				continue
			}
			retry = append(retry, leaseUpdate{ref: u.ref, oid: u.oid, old: remote})
			retryAt = append(retryAt, i)
		default:
			b.refused[u.ref] = u.oid
			errs[i] = rejected(u.ref, r, rep)
		}
	}
	if len(retry) == 0 {
		return errs, nil
	}
	res, rep, err = b.pushLeased(ctx, retry)
	if err != nil {
		return errs, err
	}
	for j, u := range retry {
		i := retryAt[j]
		switch r := res[j]; {
		case accepted(r):
			errs[i] = b.recordPushed(ctx, u.ref, u.oid, true)
		case r.Kind == gitx.RejectStale:
			errs[i] = fmt.Errorf("%w: %s changed again while retrying", ErrIdentityCollision, u.ref)
		default:
			b.refused[u.ref] = u.oid
			errs[i] = rejected(u.ref, r, rep)
		}
	}
	return errs, nil
}

// pushOwnOne is pushOwn for a single update.
func (b *Bus) pushOwnOne(ctx context.Context, u leaseUpdate) error {
	errs, err := b.pushOwn(ctx, []leaseUpdate{u})
	if err != nil {
		return err
	}
	return errs[0]
}

// ownLease returns the lease for pushing an own ref: the oid last pushed.
func (b *Bus) ownLease(ctx context.Context, ref string) (string, error) {
	last, err := b.pushLog.LastPushed(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("last push of %s: %w", ref, err)
	}
	return last, nil
}

// fetchExact fetches one ref by exact name into its tracking ref and returns
// its remote oid, or "" (removing the tracking ref) when the remote does not
// have it.
func (b *Bus) fetchExact(ctx context.Context, ref string) (string, error) {
	tr := b.tracking(ref)
	err := b.withFetchLock(ctx, func() error {
		return b.repo.Fetch(ctx, gitx.FetchOptions{
			Remote:   b.remoteName,
			Refspecs: []string{"+" + ref + ":" + tr},
			Metadata: true,
		})
	})
	if isMissingRemoteRef(err) {
		_ = b.repo.DeleteRef(ctx, tr, "")
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return b.localOID(ctx, tr)
}

// isMissingRemoteRef reports whether a fetch failed because a ref named
// exactly in a refspec does not exist on the remote.
func isMissingRemoteRef(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gitx.ErrNotFound) {
		return true
	}
	var ge *gitx.Error
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref")
}

// withFetchLock runs fn holding the clone-wide fetch lock (docs/SPEC.md,
// section 6.2), waiting for it until ctx is done.
func (b *Bus) withFetchLock(ctx context.Context, fn func() error) error {
	if b.lockDir == "" {
		return fn()
	}
	l, err := flock.Acquire(ctx, filepath.Join(b.lockDir, "fetch.lock"))
	if err != nil {
		return fmt.Errorf("fetch lock: %w", err)
	}
	defer l.Unlock()
	return fn()
}

// claimPushError maps a failed claim push: an unreachable remote matches
// ErrUnreachable (and still the transport error), never falling back to a
// local compare-and-swap.
func (b *Bus) claimPushError(op string, err error) error {
	if !errors.Is(err, ErrObserver) && b.isNetwork(err) {
		return fmt.Errorf("bus: %s: %w: %w", op, ErrUnreachable, err)
	}
	return fmt.Errorf("bus: %s: %w", op, err)
}
