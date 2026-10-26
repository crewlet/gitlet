package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

func (b *Bus) readOwnState(ctx context.Context) (model.AgentState, string, error) {
	var st model.AgentState
	oid, err := b.readOwn(ctx, StateRef(b.me), stateFile, func(data []byte) (err error) {
		st, _, err = decodeState(b.me, data)
		return err
	})
	if err != nil {
		return model.AgentState{}, oid, fmt.Errorf("bus: read own state: %w", err)
	}
	return st, oid, nil
}

func (b *Bus) readOwnOutbox(ctx context.Context) (model.Outbox, string, error) {
	var ob model.Outbox
	oid, err := b.readOwn(ctx, OutboxRef(b.me), outboxFile, func(data []byte) (err error) {
		ob, err = decodeOutbox(b.me, data)
		return err
	})
	if err != nil {
		return model.Outbox{}, oid, fmt.Errorf("bus: read own outbox: %w", err)
	}
	return ob, oid, nil
}

// readOwn reads one of this agent's documents from its local ref and passes
// it to decode. It returns the ref's oid even when the document cannot be
// decoded, so the caller can still replace it.
func (b *Bus) readOwn(ctx context.Context, ref, file string, decode func([]byte) error) (string, error) {
	oid, err := b.localOID(ctx, ref)
	if err != nil || oid == "" {
		return "", err
	}
	data, err := b.readDoc(ctx, oid, file)
	if err != nil {
		return oid, err
	}
	return oid, decode(data)
}

func (b *Bus) writeState(ctx context.Context, st model.AgentState, seenOID string) (string, error) {
	_, data, err := encodeState(b.me, st, b.now())
	if err != nil {
		return "", fmt.Errorf("bus: write state: %w", err)
	}
	return b.writeOwnDoc(ctx, StateRef(b.me), stateFile, stateMessage, data, seenOID)
}

func (b *Bus) writeOutbox(ctx context.Context, ob model.Outbox, seenOID string) (string, error) {
	_, data, err := encodeOutbox(b.me, ob)
	if err != nil {
		return "", fmt.Errorf("bus: write outbox: %w", err)
	}
	return b.writeOwnDoc(ctx, OutboxRef(b.me), outboxFile, outboxMessage, data, seenOID)
}

// writeOwnDoc commits a document, moves the local ref from seenOID to it and
// pushes it. Once the local write is done its oid is returned, even when the
// push fails: the local ref is the agent's state and Flush publishes it later.
func (b *Bus) writeOwnDoc(ctx context.Context, ref, file, message string, data []byte, seenOID string) (string, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	oid, err := b.commitDoc(ctx, file, data, message, "")
	if err != nil {
		return "", fmt.Errorf("bus: write %s: %w", ref, err)
	}
	if err := b.casLocal(ctx, ref, oid, seenOID); err != nil {
		return "", fmt.Errorf("bus: write %s: %w", ref, err)
	}
	if err := b.publishOwn(ctx, ref, oid); err != nil {
		return oid, fmt.Errorf("bus: push %s: %w", ref, err)
	}
	return oid, nil
}

// publishOwn pushes the new value of an own ref with a lease on the oid last
// pushed. It does nothing without a remote.
func (b *Bus) publishOwn(ctx context.Context, ref, oid string) error {
	if b.remoteName == "" {
		return nil
	}
	last, err := b.ownLease(ctx, ref)
	if err != nil {
		return err
	}
	return b.pushOwnOne(ctx, leaseUpdate{ref: ref, oid: oid, old: last})
}

// casLocal moves a local own ref from seen ("" when it must not exist) to oid.
func (b *Bus) casLocal(ctx context.Context, ref, oid, seen string) error {
	conflict, err := b.swapLocal(ctx, ref, oid, seen)
	if conflict {
		return fmt.Errorf("changed since it was read at %q: %w", seen, err)
	}
	return err
}

// swapLocal moves a local ref from seen ("" when it must not exist) to oid
// ("" deletes it, and then seen must be set). conflict reports that the
// update failed because the ref was not at seen.
func (b *Bus) swapLocal(ctx context.Context, ref, oid, seen string) (conflict bool, err error) {
	switch {
	case oid == "":
		err = b.repo.DeleteRef(ctx, ref, seen)
	case seen == "":
		err = b.repo.UpdateRef(ctx, ref, oid, gitx.ZeroOID)
	default:
		err = b.repo.UpdateRef(ctx, ref, oid, seen)
	}
	if err == nil {
		return false, nil
	}
	cur, rerr := b.localOID(ctx, ref)
	return rerr == nil && cur != seen, err
}

func (b *Bus) writeWip(ctx context.Context, item model.ItemID, commit string) error {
	if !model.ValidItemID(item) {
		return fmt.Errorf("bus: write wip: invalid item id %q", item)
	}
	if !validOID(commit) {
		return fmt.Errorf("bus: write wip: invalid commit %q", commit)
	}
	ref := WipRef(b.me, item)
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if err := b.repo.UpdateRef(ctx, ref, commit, ""); err != nil {
		return fmt.Errorf("bus: write %s: %w", ref, err)
	}
	// publishOwn pushes the snapshot on its own, never batched with
	// metadata (docs/SPEC.md, section 6.1).
	if err := b.publishOwn(ctx, ref, commit); err != nil {
		return fmt.Errorf("bus: push %s: %w", ref, err)
	}
	return nil
}

func (b *Bus) deleteWip(ctx context.Context, item model.ItemID) error {
	if !model.ValidItemID(item) {
		return fmt.Errorf("bus: delete wip: invalid item id %q", item)
	}
	ref := WipRef(b.me, item)
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if err := b.deleteLocal(ctx, ref); err != nil {
		return fmt.Errorf("bus: delete %s: %w", ref, err)
	}
	if err := b.unpublishOwn(ctx, []string{ref}); err != nil {
		return fmt.Errorf("bus: delete %s: %w", ref, err)
	}
	return nil
}

func (b *Bus) deleteOwn(ctx context.Context) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	local, err := b.ownRefs(ctx, false)
	if err != nil {
		return fmt.Errorf("bus: delete own refs: %w", err)
	}
	all := map[string]bool{}
	var errs []error
	for _, ref := range sortedKeys(local) {
		all[ref] = true
		if err := b.repo.DeleteRef(ctx, ref, local[ref]); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", ref, err))
		}
	}
	if b.remoteName != "" {
		tracked, err := b.ownRefs(ctx, true)
		if err != nil {
			return fmt.Errorf("bus: delete own refs: %w", errors.Join(append(errs, err)...))
		}
		for ref := range tracked {
			all[ref] = true
		}
		if err := b.unpublishOwn(ctx, sortedKeys(all)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("bus: delete own refs: %w", err)
	}
	return nil
}

// deleteLocal deletes a local ref if it exists.
func (b *Bus) deleteLocal(ctx context.Context, ref string) error {
	cur, err := b.localOID(ctx, ref)
	if err != nil || cur == "" {
		return err
	}
	return b.repo.DeleteRef(ctx, ref, cur)
}

// unpublishOwn deletes own refs on the remote in one push (deletions carry
// no objects, so batching them cannot hold anything up). Each deletion has a
// lease on the oid last pushed or, failing that, on the last remote oid
// seen; refs with neither were never published from here and are left alone.
func (b *Bus) unpublishOwn(ctx context.Context, refs []string) error {
	if b.remoteName == "" {
		return nil
	}
	var ups []leaseUpdate
	for _, ref := range refs {
		old, err := b.ownLease(ctx, ref)
		if err == nil && old == "" {
			old, err = b.localOID(ctx, b.tracking(ref))
		}
		if err != nil {
			return err
		}
		if old != "" {
			ups = append(ups, leaseUpdate{ref: ref, old: old})
		}
	}
	if len(ups) == 0 {
		return nil
	}
	errs, err := b.pushOwn(ctx, ups)
	if err != nil {
		return err
	}
	return errors.Join(errs...)
}

func (b *Bus) flush(ctx context.Context) error {
	if b.remoteName == "" {
		return nil
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if err := b.writable(); err != nil {
		return fmt.Errorf("bus: flush: %w", err)
	}
	local, err := b.ownRefs(ctx, false)
	if err != nil {
		return fmt.Errorf("bus: flush: %w", err)
	}
	tracked, err := b.ownRefs(ctx, true)
	if err != nil {
		return fmt.Errorf("bus: flush: %w", err)
	}
	all := map[string]bool{}
	for ref := range local {
		all[ref] = true
	}
	for ref := range tracked {
		all[ref] = true
	}
	var meta, wips []leaseUpdate
	for _, ref := range sortedKeys(all) {
		last, err := b.ownLease(ctx, ref)
		if err != nil {
			return fmt.Errorf("bus: flush: %w", err)
		}
		loc := local[ref]
		_, published := tracked[ref]
		var u leaseUpdate
		switch {
		case loc != "" && loc != last:
			// Written while the remote was unreachable.
			u = leaseUpdate{ref: ref, oid: loc, old: last}
		case loc != "" && !published && !isWipRef(ref):
			// The poll pruned the tracking ref, so the remote lost the ref
			// (reaped or deleted by hand): recreate it create-only. Wip
			// tracking refs are not polled, so this is only known for
			// state and outbox.
			u = leaseUpdate{ref: ref, oid: loc}
		case loc == "" && last != "":
			// Deleted while the remote was unreachable.
			u = leaseUpdate{ref: ref, old: last}
		default:
			continue
		}
		if isWipRef(ref) {
			wips = append(wips, u)
		} else {
			meta = append(meta, u)
		}
	}
	var errs []error
	if len(meta) > 0 {
		perRef, err := b.pushOwn(ctx, meta)
		if err != nil {
			return fmt.Errorf("bus: flush: %w", err)
		}
		errs = append(errs, perRef...)
	}
	for _, u := range wips {
		// Each snapshot in a push of its own (docs/SPEC.md, section 6.1).
		perRef, err := b.pushOwn(ctx, []leaseUpdate{u})
		if err != nil {
			return fmt.Errorf("bus: flush: %w", errors.Join(append(errs, err)...))
		}
		errs = append(errs, perRef...)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("bus: flush: %w", err)
	}
	return nil
}
