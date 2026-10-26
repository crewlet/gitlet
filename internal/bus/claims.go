package bus

import (
	"context"
	"fmt"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

func (b *Bus) createClaim(ctx context.Context, c model.Claim, parent string) (bool, ClaimView, error) {
	op := "claim " + c.Key
	c, oid, err := b.commitClaim(ctx, c, parent)
	if err != nil {
		return false, ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	ref := ClaimRef(c.Key)
	if b.remoteName == "" {
		return b.createLocalClaim(ctx, op, ref, c, oid)
	}
	if err := b.writable(); err != nil {
		return false, ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	res, rep, err := b.pushLeased(ctx, []leaseUpdate{{ref: ref, oid: oid}})
	if err != nil {
		return false, ClaimView{}, b.claimPushError(op, err)
	}
	switch r := res[0]; {
	case accepted(r):
		_ = b.recordPushed(ctx, ref, oid, false)
		return true, ClaimView{Claim: c, OID: oid}, nil
	case r.Kind == gitx.RejectStale:
		// Lost the race: fetch only this ref to learn the holder.
		view, err := b.fetchClaim(ctx, c.Key)
		if err != nil {
			return false, view, b.claimPushError(op+": held, but reading it failed", err)
		}
		return false, view, nil
	default:
		return false, ClaimView{}, fmt.Errorf("bus: %s: %w", op, rejected(ref, r, rep))
	}
}

// createLocalClaim is CreateClaim without a remote: the shared .git referees
// through update-ref's old value (docs/SPEC.md, section 6.3).
func (b *Bus) createLocalClaim(ctx context.Context, op, ref string, c model.Claim, oid string) (bool, ClaimView, error) {
	conflict, err := b.swapLocal(ctx, ref, oid, "")
	if err == nil {
		return true, ClaimView{Claim: c, OID: oid}, nil
	}
	if !conflict {
		return false, ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	cur, err := b.localOID(ctx, ref)
	if err != nil {
		return false, ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	view, err := b.readClaim(ctx, c.Key, cur)
	if err != nil {
		return false, view, fmt.Errorf("bus: %s: held, but %w", op, err)
	}
	return false, view, nil
}

func (b *Bus) updateClaim(ctx context.Context, c model.Claim, seenOID, parent string) (ClaimView, error) {
	op := "update claim " + c.Key
	if !validOID(seenOID) {
		return ClaimView{}, fmt.Errorf("bus: %s: invalid seen oid %q", op, seenOID)
	}
	c, oid, err := b.commitClaim(ctx, c, parent)
	if err != nil {
		return ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	ref := ClaimRef(c.Key)
	if b.remoteName == "" {
		if err := b.casClaim(ctx, ref, oid, seenOID); err != nil {
			return ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
		}
		return ClaimView{Claim: c, OID: oid}, nil
	}
	if err := b.writable(); err != nil {
		return ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	res, rep, err := b.pushLeased(ctx, []leaseUpdate{{ref: ref, oid: oid, old: seenOID}})
	if err != nil {
		return ClaimView{}, b.claimPushError(op, err)
	}
	if err := b.claimResult(ctx, ref, oid, res[0], rep); err != nil {
		return ClaimView{}, fmt.Errorf("bus: %s: %w", op, err)
	}
	return ClaimView{Claim: c, OID: oid}, nil
}

func (b *Bus) updateClaims(ctx context.Context, updates []ClaimUpdate) ([]ClaimOutcome, error) {
	out := make([]ClaimOutcome, len(updates))
	ups := make([]leaseUpdate, len(updates))
	seen := make(map[string]bool, len(updates))
	for i, u := range updates {
		op := "update claim " + u.Claim.Key
		if seen[u.Claim.Key] {
			return nil, fmt.Errorf("bus: %s: listed twice", op)
		}
		seen[u.Claim.Key] = true
		if !validOID(u.SeenOID) {
			return nil, fmt.Errorf("bus: %s: invalid seen oid %q", op, u.SeenOID)
		}
		c, oid, err := b.commitClaim(ctx, u.Claim, u.Parent)
		if err != nil {
			return nil, fmt.Errorf("bus: %s: %w", op, err)
		}
		out[i] = ClaimOutcome{Key: c.Key, View: ClaimView{Claim: c, OID: oid}}
		ups[i] = leaseUpdate{ref: ClaimRef(c.Key), oid: oid, old: u.SeenOID}
	}
	if len(updates) == 0 {
		return out, nil
	}
	if b.remoteName == "" {
		for i, u := range ups {
			if err := b.casClaim(ctx, u.ref, u.oid, u.old); err != nil {
				out[i].View, out[i].Err = ClaimView{}, err
			}
		}
		return out, nil
	}
	if err := b.writable(); err != nil {
		return nil, fmt.Errorf("bus: update claims: %w", err)
	}
	// One push with a lease per ref and no --atomic (docs/SPEC.md, section 6.1).
	res, rep, err := b.pushLeased(ctx, ups)
	if err != nil {
		return nil, b.claimPushError("update claims", err)
	}
	for i, u := range ups {
		if err := b.claimResult(ctx, u.ref, u.oid, res[i], rep); err != nil {
			out[i].View, out[i].Err = ClaimView{}, err
		}
	}
	return out, nil
}

func (b *Bus) deleteClaim(ctx context.Context, key, seenOID string) error {
	op := "delete claim " + key
	if !validKey(key) {
		return fmt.Errorf("bus: %s: invalid symbol key", op)
	}
	if !validOID(seenOID) {
		return fmt.Errorf("bus: %s: invalid seen oid %q", op, seenOID)
	}
	ref := ClaimRef(key)
	if b.remoteName == "" {
		if err := b.casClaim(ctx, ref, "", seenOID); err != nil {
			return fmt.Errorf("bus: %s: %w", op, err)
		}
		return nil
	}
	if err := b.writable(); err != nil {
		return fmt.Errorf("bus: %s: %w", op, err)
	}
	res, rep, err := b.pushLeased(ctx, []leaseUpdate{{ref: ref, old: seenOID}})
	if err != nil {
		return b.claimPushError(op, err)
	}
	if err := b.claimResult(ctx, ref, "", res[0], rep); err != nil {
		return fmt.Errorf("bus: %s: %w", op, err)
	}
	return nil
}

// commitClaim validates and encodes a claim and writes its commit, with
// parent (a stub) as its only parent when set.
func (b *Bus) commitClaim(ctx context.Context, c model.Claim, parent string) (model.Claim, string, error) {
	if parent != "" && !validOID(parent) {
		return c, "", fmt.Errorf("invalid parent %q", parent)
	}
	c, data, err := encodeClaim(c)
	if err != nil {
		return c, "", err
	}
	oid, err := b.commitDoc(ctx, claimFile, data, claimMessage, parent)
	return c, oid, err
}

// claimResult turns the result of a leased claim update or delete into an
// error: nil when accepted, ErrClaimLost when the lease was stale, and a
// *RejectedError otherwise.
func (b *Bus) claimResult(ctx context.Context, ref, oid string, r gitx.PushResult, rep gitx.PushReport) error {
	switch {
	case accepted(r):
		_ = b.recordPushed(ctx, ref, oid, false)
		return nil
	case r.Kind == gitx.RejectStale:
		return ErrClaimLost
	default:
		return rejected(ref, r, rep)
	}
}

// casClaim is the local-mode claim write: update-ref from seen to oid (""
// deletes). A ref no longer at seen returns ErrClaimLost.
func (b *Bus) casClaim(ctx context.Context, ref, oid, seen string) error {
	conflict, err := b.swapLocal(ctx, ref, oid, seen)
	if conflict {
		return ErrClaimLost
	}
	return err
}

// fetchClaim fetches one claim ref by exact name and reads it. A claim that
// no longer exists returns a zero view.
func (b *Bus) fetchClaim(ctx context.Context, key string) (ClaimView, error) {
	oid, err := b.fetchExact(ctx, ClaimRef(key))
	if err != nil || oid == "" {
		return ClaimView{}, err
	}
	return b.readClaim(ctx, key, oid)
}

// readClaim reads and validates the claim in commit oid. On a malformed
// claim it still returns the oid.
func (b *Bus) readClaim(ctx context.Context, key, oid string) (ClaimView, error) {
	data, err := b.readDoc(ctx, oid, claimFile)
	if err == nil {
		var c model.Claim
		if c, err = decodeClaim(key, data); err == nil {
			return ClaimView{Claim: c, OID: oid}, nil
		}
	}
	return ClaimView{OID: oid}, fmt.Errorf("claim %s at %s: %w", key, oid, err)
}
