package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

const (
	// itemTrailer and agentTrailer are the provenance lines submit writes
	// into PR bodies and the prepare-commit-msg hook into commits.
	itemTrailer  = "Gitlet-Item"
	agentTrailer = "Gitlet-Agent"
	// ghTimeout bounds one gh call; gh is never required.
	ghTimeout = 5 * time.Second
	// ghEvery is how often gh is asked about one item's pull request.
	ghEvery = 5 * time.Minute
)

// detectLanding checks my submitted items against main and lands those
// that are in (docs/SPEC.md, section 7). Tick runs it; update and submit
// may run it after moving main.
func (e *Engine) detectLanding(ctx context.Context) error {
	main := e.mainOID(ctx)
	if main == "" || e.readOnly() {
		return nil
	}
	var errs []error
	for _, it := range e.myState().Items {
		if it.Status == model.ItemSubmitted && e.itemLanded(ctx, it, main) {
			if err := e.land(ctx, it); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// itemLanded judges landing by content, so squash and rebase merges count.
// The item has landed when its head is an ancestor of main; when merging
// head into main changes nothing (git merge-tree yields main's own tree);
// when a commit on main since the item's base carries "Gitlet-Item: <id>"
// in its message; or when gh reports its pull request merged. Negative git
// answers are memoized per (item, head, main).
func (e *Engine) itemLanded(ctx context.Context, it model.WorkItem, main string) bool {
	head := it.Head
	if head == "" {
		return false
	}
	k := string(it.ID) + "\x00" + head + "\x00" + main
	if done, ok := e.landChecks[k]; ok && !done {
		return e.ghMerged(ctx, it)
	}
	if e.hasCommit(ctx, head) {
		if ok, err := e.bg.IsAncestor(ctx, head, main); err == nil && ok {
			return true
		}
		if e.gitOK {
			if res, err := e.bg.MergeTree(ctx, main, head); err == nil && res.Clean {
				if tree, err := e.treeOf(ctx, main); err == nil && tree == res.Tree {
					return true
				}
			}
		}
	}
	if it.Base != "" && it.Base != main {
		oids, err := e.bg.LogGrep(ctx, it.Base+".."+main, itemTrailer+": "+string(it.ID))
		if err == nil && len(oids) > 0 {
			return true
		}
	}
	if ctx.Err() == nil {
		e.landChecks[k] = false
	}
	return e.ghMerged(ctx, it)
}

// ghMerged asks gh whether the item's pull request was merged, at most
// every ghEvery per item and within ghTimeout. A missing, unauthenticated
// or slow gh means "not known". A pull request closed without merging is
// remembered for status, which offers to abandon the task (prClosed).
func (e *Engine) ghMerged(ctx context.Context, it model.WorkItem) bool {
	if it.PR == "" || e.bus.Remote() == "" {
		return false
	}
	now := e.now()
	if t, ok := e.ghChecked[it.ID]; ok && now.Sub(t) < ghEvery {
		return false
	}
	e.ghChecked[it.ID] = now
	gh, err := exec.LookPath("gh")
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, gh, "pr", "view", it.PR, "--json", "state", "--jq", ".state")
	cmd.Dir = e.repo.Root
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	switch {
	case state == "CLOSED":
		e.rememberPRClosed(ctx, it.ID, it.PR)
	case e.prClosed(ctx, it):
		e.rememberPRClosed(ctx, it.ID, "") // reopened
	}
	return state == "MERGED"
}

func (e *Engine) prClosedKey(item model.ItemID) string {
	return "pr-closed/" + string(e.me.ID) + "/" + string(item)
}

// rememberPRClosed records that gh reported pr, the item's pull request,
// closed without merging, or with pr "" forgets it. The ledger keeps it, so
// status shows it in every process of the worktree: a CLI call asks gh
// only in its upkeep, after its reply, and a CLI call beside a live session
// leaves gh to the session's ticks. Best effort.
func (e *Engine) rememberPRClosed(ctx context.Context, item model.ItemID, pr string) {
	if e.led.ReadOnly() {
		return
	}
	var err error
	if pr != "" {
		err = e.led.Set(ctx, e.prClosedKey(item), pr)
	} else {
		err = e.led.Delete(ctx, e.prClosedKey(item))
	}
	if err != nil {
		e.log.Debug("remember closed pull request", "item", item, "err", err)
	}
}

// prClosed reports whether gh last reported the pull request of a submitted
// item closed without merging.
func (e *Engine) prClosed(ctx context.Context, it model.WorkItem) bool {
	if it.Status != model.ItemSubmitted || it.PR == "" {
		return false
	}
	pr, ok, err := e.led.Get(ctx, e.prClosedKey(it.ID))
	return err == nil && ok && pr == it.PR
}

// land marks a submitted item landed and drops what it leaves behind
// (releaseItem).
func (e *Engine) land(ctx context.Context, it model.WorkItem) error {
	now := e.now().UTC()
	_, err := e.mutateState(ctx, func(st *model.AgentState) error {
		i := itemIndex(st.Items, it.ID)
		if i < 0 || st.Items[i].Status != model.ItemSubmitted {
			return errNoChange
		}
		st.Items[i].Status = model.ItemLanded
		st.Items[i].ClosedAt = &now
		st.Items[i].UpdatedAt = now
		return nil
	})
	if err != nil {
		return fmt.Errorf("engine: land %s: %w", it.ID, err)
	}
	if _, err := e.releaseItem(ctx, it.ID); err != nil {
		return err
	}
	e.appendEvent(ctx, "landed", map[string]any{"item": it.ID, "intent": it.Intent, "pr": it.PR})
	return nil
}

// releaseItem drops what an item that landed or was abandoned leaves
// behind (docs/SPEC.md, section 7): the claims I hold for it, each deleted
// with a lease on the oid seen; its needs in my outbox; its wip ref; and
// what gh said about its pull request.
// It returns what it released ("claim <symbol>", "need <symbol>"). Claim
// and wip deletions that fail are logged and left to later ticks (the
// rules' Releases retry claims); an outbox write that fails is an error.
// abandon uses it too.
func (e *Engine) releaseItem(ctx context.Context, item model.ItemID) ([]string, error) {
	e.rememberFinished(ctx, item)
	e.rememberPRClosed(ctx, item, "")
	var released []string
	for _, cv := range e.myClaims() {
		if cv.Claim.Item != item {
			continue
		}
		if err := e.deleteClaim(ctx, cv.Claim.Key, cv.OID); err != nil {
			e.log.Debug("release claim", "symbol", cv.Claim.Symbol.Ref(), "err", err)
			continue
		}
		released = append(released, "claim "+cv.Claim.Symbol.Ref())
	}
	var needs []string
	_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		kept := ob.Needs[:0:0]
		needs = nil
		for _, n := range ob.Needs {
			if n.Item == item {
				needs = append(needs, "need "+n.Name)
				continue
			}
			kept = append(kept, n)
		}
		if len(needs) == 0 {
			return errNoChange
		}
		ob.Needs = kept
		return nil
	})
	if err != nil {
		return released, fmt.Errorf("engine: release needs of %s: %w", item, err)
	}
	released = append(released, needs...)
	if err := e.bus.DeleteWip(ctx, item); err != nil {
		e.log.Debug("delete wip", "item", item, "err", err)
		if gitx.IsNetwork(err) {
			e.poll.markOffline(e.now())
		}
	}
	return released, nil
}

// createClaim creates a claim with a create-only push (the remote referees;
// docs/SPEC.md, section 6.1) and caches the claim as it now stands, whoever
// won. parent, when set, becomes the claim commit's parent (a stub). Without
// a reachable remote it returns bus.ErrUnreachable: claims never fall back
// to local compare-and-swap. writeRefusal maps its errors for verbs.
func (e *Engine) createClaim(ctx context.Context, c model.Claim, parent string) (bool, bus.ClaimView, error) {
	if e.readOnly() {
		return false, bus.ClaimView{}, e.readOnlyErr()
	}
	won, cur, err := e.bus.CreateClaim(ctx, c, parent)
	if err == nil && !won && cur.OID == "" {
		// Lost, but the holder's claim vanished before we could read it.
		won, cur, err = e.bus.CreateClaim(ctx, c, parent)
	}
	if err != nil {
		return false, cur, e.claimErr(ctx, "create", c.Key, err)
	}
	if !won && cur.Claim.Owner == e.me.ID {
		won = true // our create went through but its reply was lost
	}
	if cur.OID != "" {
		e.setClaim(c.Key, &cur)
	}
	return won, cur, nil
}

// updateClaim replaces the claim seen at seenOID with c (renewal, contract,
// requirements, transfer with epoch + 1, takeover of a lapsed claim) and
// caches the result. When the claim changed meanwhile it returns
// bus.ErrClaimLost and reloads the shared state, so the caller sees the
// current holder.
func (e *Engine) updateClaim(ctx context.Context, c model.Claim, seenOID, parent string) (bus.ClaimView, error) {
	if e.readOnly() {
		return bus.ClaimView{}, e.readOnlyErr()
	}
	cur, err := e.bus.UpdateClaim(ctx, c, seenOID, parent)
	if err != nil {
		return cur, e.claimErr(ctx, "update", c.Key, err)
	}
	e.setClaim(c.Key, &cur)
	return cur, nil
}

// deleteClaim deletes the claim on key if it is still at seenOID and drops
// it from the cache. A claim that is already gone counts as deleted; one
// that changed meanwhile returns bus.ErrClaimLost, after a reload.
func (e *Engine) deleteClaim(ctx context.Context, key, seenOID string) error {
	if e.readOnly() {
		return e.readOnlyErr()
	}
	if err := e.bus.DeleteClaim(ctx, key, seenOID); err != nil {
		err = e.claimErr(ctx, "delete", key, err)
		if _, still := e.claimFor(key); errors.Is(err, bus.ErrClaimLost) && !still {
			return nil
		}
		return err
	}
	e.setClaim(key, nil)
	return nil
}

// claimErr classifies a claim write error and keeps the engine's view
// consistent with it.
func (e *Engine) claimErr(ctx context.Context, op, key string, err error) error {
	switch {
	case errors.Is(err, bus.ErrClaimLost):
		if rerr := e.reload(ctx); rerr != nil {
			e.log.Debug("reload after lost claim", "err", rerr)
		}
	case errors.Is(err, bus.ErrUnreachable), gitx.IsNetwork(err):
		e.poll.markOffline(e.now())
	case errors.Is(err, bus.ErrObserver):
		e.enterObserver()
	}
	return fmt.Errorf("engine: %s claim %s: %w", op, key, err)
}

// newClaim builds a claim on sym owned by this agent for item, at epoch 0.
func (e *Engine) newClaim(sym model.Symbol, item model.ItemID, auto bool, reqs []model.Requirement) model.Claim {
	if sym.Key == "" {
		sym.Key = model.SymbolKey(sym.Lang, sym.Path, sym.Qualified)
	}
	return model.Claim{
		Format:       model.Format,
		Key:          sym.Key,
		Symbol:       sym,
		Owner:        e.me.ID,
		Item:         item,
		LeaseSeconds: leaseSeconds(e.cfg.Lease),
		Auto:         auto,
		Requirements: reqs,
		UpdatedAt:    e.now().UTC(),
	}
}
