package bus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

func TestCreateClaimRace(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repos := []*fakeRepo{e.clone(), e.clone()}
	agents := []model.AgentID{agentA, agentB}
	items := []model.ItemID{itemA, itemB}
	type result struct {
		won  bool
		view ClaimView
		err  error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range repos {
		b := e.bus(repos[i], agents[i])
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			won, view, err := b.CreateClaim(ctx, sampleClaim(keyA, agents[i], items[i]), "")
			results[i] = result{won, view, err}
		}()
	}
	close(start)
	wg.Wait()

	winner, loser := 0, 1
	if results[1].won {
		winner, loser = 1, 0
	}
	for i, r := range results {
		mustNoErr(t, r.err)
		if i != winner && r.won {
			t.Fatal("both won")
		}
	}
	if !results[winner].won {
		t.Fatal("nobody won")
	}
	w, l := results[winner], results[loser]
	if e.remote.get(ClaimRef(keyA)) != w.view.OID || w.view.Claim.Owner != agents[winner] {
		t.Fatalf("remote %q, winner view %+v", e.remote.get(ClaimRef(keyA)), w.view)
	}
	// The loser learned the holder by fetching only that ref.
	if l.view.OID != w.view.OID || l.view.Claim.Owner != agents[winner] || l.view.Claim.Item != items[winner] {
		t.Fatalf("loser view = %+v, want the winner's claim", l.view)
	}
	fetches := repos[loser].fetchLog()
	want := []string{"+" + ClaimRef(keyA) + ":refs/gitlet-remote/origin/claims/" + keyA}
	if len(fetches) != 1 || !reflect.DeepEqual(fetches[0].Refspecs, want) {
		t.Fatalf("loser fetches = %+v, want %v", fetches, want)
	}
	for i, r := range repos {
		if got := r.ref("refs/gitlet-remote/origin/claims/" + keyA); got != w.view.OID {
			t.Errorf("clone %d tracking ref = %q, want %q", i, got, w.view.OID)
		}
		push := r.pushLog()[0]
		if push.Updates[0].Expect != gitx.ExpectAbsent || !push.Metadata {
			t.Errorf("clone %d create push = %+v", i, push)
		}
		if r.ref(ClaimRef(keyA)) != "" {
			t.Errorf("clone %d wrote a local claim ref", i)
		}
	}
}

func TestCreateClaimWithStub(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	b := e.bus(e.clone(), agentA)
	stub := e.store.commit("src/config.ts", []byte("stub"), "gitlet stub: parseConfig", e.remote.get("refs/heads/main"))
	c := sampleClaim(keyA, agentA, itemA)
	c.Contract = &model.Contract{Signature: "function parseConfig(string, boolean?): Config", SigHash: "h", StubCommit: stub, PublishedAt: t0}
	won, view, err := b.CreateClaim(ctx, c, stub)
	mustNoErr(t, err)
	if !won || view.Claim.Format != model.Format {
		t.Fatalf("won=%v view=%+v", won, view)
	}
	commit := e.store.get(view.OID)
	if commit.message != "gitlet: claim" || !reflect.DeepEqual(commit.parents, []string{stub}) {
		t.Fatalf("claim commit message/parents = %q/%v", commit.message, commit.parents)
	}
	if _, _, err := b.CreateClaim(ctx, sampleClaim("not-a-key", agentA, itemA), ""); err == nil {
		t.Fatal("invalid key accepted")
	}
	if _, _, err := b.CreateClaim(ctx, sampleClaim(keyB, agentA, itemA), "HEAD"); err == nil {
		t.Fatal("invalid parent accepted")
	}
}

func TestUpdateAndDeleteClaimLease(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repoA := e.clone()
	a, b := e.bus(repoA, agentA), e.bus(e.clone(), agentB)
	won, v1, err := a.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	mustNoErr(t, err)
	if !won {
		t.Fatal("create lost")
	}

	// Renewal by the holder.
	c := v1.Claim
	c.UpdatedAt = t0.Add(time.Minute)
	v2, err := a.UpdateClaim(ctx, c, v1.OID, "")
	mustNoErr(t, err)
	if e.remote.get(ClaimRef(keyA)) != v2.OID || repoA.ref(a.tracking(ClaimRef(keyA))) != v2.OID {
		t.Fatal("renewal not applied")
	}
	if push := repoA.pushLog()[1]; push.Updates[0].Expect != gitx.ExpectOID || push.Updates[0].OldOID != v1.OID {
		t.Fatalf("renewal push = %+v", push.Updates[0])
	}

	// Takeover by B on the oid judged lapsed.
	c.Owner, c.Item, c.Epoch = agentB, itemB, 1
	v3, err := b.UpdateClaim(ctx, c, v2.OID, "")
	mustNoErr(t, err)

	// A's next write is refused the same way: that is how it learns.
	c.Owner, c.Item, c.Epoch = agentA, itemA, 0
	if _, err := a.UpdateClaim(ctx, c, v2.OID, ""); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale update: %v, want ErrClaimLost", err)
	}
	if err := a.DeleteClaim(ctx, keyA, v2.OID); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale delete: %v, want ErrClaimLost", err)
	}
	if e.remote.get(ClaimRef(keyA)) != v3.OID {
		t.Fatal("stale writes changed the claim")
	}
	mustNoErr(t, b.DeleteClaim(ctx, keyA, v3.OID))
	if e.remote.get(ClaimRef(keyA)) != "" {
		t.Fatal("claim not deleted")
	}
	// Deleting a claim that is gone is a lost lease too.
	if err := b.DeleteClaim(ctx, keyA, v3.OID); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("delete of missing claim: %v", err)
	}
	if _, err := a.UpdateClaim(ctx, c, "", ""); err == nil {
		t.Fatal("update without a seen oid accepted")
	}
}

func TestUpdateClaimsMixedOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repoA := e.clone()
	a, b := e.bus(repoA, agentA), e.bus(e.clone(), agentB)
	views := map[string]ClaimView{}
	for _, key := range []string{keyA, keyB, keyC} {
		_, v, err := a.CreateClaim(ctx, sampleClaim(key, agentA, itemA), "")
		mustNoErr(t, err)
		views[key] = v
	}
	// B took keyB over; the remote refuses writes to keyC.
	taken := sampleClaim(keyB, agentB, itemB)
	taken.Epoch = 1
	_, err := b.UpdateClaim(ctx, taken, views[keyB].OID, "")
	mustNoErr(t, err)
	e.remote.setReject(func(dst, cur string) (string, []string, bool) {
		if dst == ClaimRef(keyC) {
			return "protected ref", []string{"refs/gitlet/claims/ffeeddccbbaa9988 is protected"}, true
		}
		return "", nil, false
	})

	var ups []ClaimUpdate
	for _, key := range []string{keyA, keyB, keyC} {
		c := views[key].Claim
		c.UpdatedAt = t0.Add(5 * time.Minute)
		ups = append(ups, ClaimUpdate{Claim: c, SeenOID: views[key].OID})
	}
	before := len(repoA.pushLog())
	out, err := a.UpdateClaims(ctx, ups)
	mustNoErr(t, err)
	pushes := repoA.pushLog()[before:]
	if len(pushes) != 1 || len(pushes[0].Updates) != 3 {
		t.Fatalf("pushes = %+v, want one push of three updates", pushes)
	}
	for i, u := range pushes[0].Updates {
		if u.Expect != gitx.ExpectOID || u.OldOID != ups[i].SeenOID {
			t.Errorf("update %d lease = %+v", i, u)
		}
	}
	if len(out) != 3 || out[0].Key != keyA || out[1].Key != keyB || out[2].Key != keyC {
		t.Fatalf("outcomes = %+v", out)
	}
	if out[0].Err != nil || out[0].View.OID == "" || e.remote.get(ClaimRef(keyA)) != out[0].View.OID {
		t.Errorf("renewal = %+v", out[0])
	}
	if !errors.Is(out[1].Err, ErrClaimLost) || out[1].View.OID != "" {
		t.Errorf("taken claim = %+v, want ErrClaimLost", out[1])
	}
	var rej *RejectedError
	if !errors.As(out[2].Err, &rej) || rej.Kind != gitx.RejectRemote || len(rej.Remote) != 1 {
		t.Errorf("protected claim = %+v, want RejectedError", out[2])
	}

	if _, err := a.UpdateClaims(ctx, []ClaimUpdate{ups[0], ups[0]}); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	out, err = a.UpdateClaims(ctx, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("empty batch = %v, %v", out, err)
	}
}

func TestClaimsWithoutRemote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.local() // two worktrees of one clone share its refs
	a, b := e.bus(repo, agentA), e.bus(repo, agentB)

	won, va, err := a.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	mustNoErr(t, err)
	if !won || repo.ref(ClaimRef(keyA)) != va.OID {
		t.Fatalf("won=%v ref=%q", won, repo.ref(ClaimRef(keyA)))
	}
	won, vb, err := b.CreateClaim(ctx, sampleClaim(keyA, agentB, itemB), "")
	mustNoErr(t, err)
	if won || vb.OID != va.OID || vb.Claim.Owner != agentA {
		t.Fatalf("second create = %v %+v", won, vb)
	}
	c := va.Claim
	c.UpdatedAt = t0.Add(time.Minute)
	v2, err := a.UpdateClaim(ctx, c, va.OID, "")
	mustNoErr(t, err)
	if _, err := b.UpdateClaim(ctx, sampleClaim(keyA, agentB, itemB), va.OID, ""); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale update: %v", err)
	}
	out, err := a.UpdateClaims(ctx, []ClaimUpdate{{Claim: c, SeenOID: va.OID}})
	mustNoErr(t, err)
	if !errors.Is(out[0].Err, ErrClaimLost) {
		t.Fatalf("stale batched update: %+v", out[0])
	}
	if err := b.DeleteClaim(ctx, keyA, va.OID); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale delete: %v", err)
	}
	mustNoErr(t, a.DeleteClaim(ctx, keyA, v2.OID))
	if repo.ref(ClaimRef(keyA)) != "" {
		t.Fatal("claim not deleted")
	}
	if len(repo.pushLog()) != 0 || len(repo.fetchLog()) != 0 {
		t.Fatal("contacted a remote")
	}
}

func TestClaimsUnreachable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	_, v, err := b.CreateClaim(ctx, sampleClaim(keyB, agentA, itemA), "")
	mustNoErr(t, err)
	e.remote.setDown(true)

	won, _, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	if won || !errors.Is(err, ErrUnreachable) || !isNet(err) {
		t.Fatalf("CreateClaim = %v, %v; want ErrUnreachable", won, err)
	}
	// Never a local fallback.
	if repo.ref(ClaimRef(keyA)) != "" || repo.ref(b.tracking(ClaimRef(keyA))) != "" {
		t.Fatal("unreachable create wrote a local claim ref")
	}
	if _, err := b.UpdateClaim(ctx, v.Claim, v.OID, ""); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("UpdateClaim: %v", err)
	}
	if _, err := b.UpdateClaims(ctx, []ClaimUpdate{{Claim: v.Claim, SeenOID: v.OID}}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("UpdateClaims: %v", err)
	}
	if err := b.DeleteClaim(ctx, keyB, v.OID); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("DeleteClaim: %v", err)
	}
}

func TestCreateClaimRejected(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	b := e.bus(e.clone(), agentA)
	e.remote.setReject(func(string, string) (string, []string, bool) {
		return "pre-receive hook declined", []string{"claims are disabled here"}, true
	})
	won, _, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	var rej *RejectedError
	if won || !errors.As(err, &rej) || rej.Remote[0] != "claims are disabled here" {
		t.Fatalf("CreateClaim = %v, %v", won, err)
	}
}

func TestCreateClaimLostToOversizedClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	// Load skips the holder's claim as too large, so the engine tries to
	// create it, loses, and reads the claim to learn the holder.
	huge := e.store.commit("claim.json", make([]byte, MaxReadDoc+1), "gitlet: claim")
	e.remote.set(ClaimRef(keyA), huge)
	won, view, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	if won || err == nil || !strings.Contains(err.Error(), "too large") || view.OID != huge {
		t.Fatalf("CreateClaim = %v, %+v, %v; want a loss with the holder's oid and a size error", won, view, err)
	}
	if n := repo.bigReadCount(); n != 0 {
		t.Fatalf("read %d documents over MaxReadDoc into memory", n)
	}
}

func TestCreateClaimLostToInvalidClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	b := e.bus(e.clone(), agentA)
	bad := e.store.commit("claim.json", []byte(`{"format":1,"key":"`+keyA+`","owner":"NOT VALID"}`), "gitlet: claim")
	e.remote.set(ClaimRef(keyA), bad)
	won, view, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	if won || err == nil || view.OID != bad {
		t.Fatalf("CreateClaim = %v, %+v, %v; want a loss with the holder's oid and an error", won, view, err)
	}
}
