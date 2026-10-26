package bus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

func TestWriteStatePushesWithLease(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	log := &memPushLog{m: map[string]string{}}
	b := e.bus(repo, agentA, withPushLog(log))
	ref := StateRef(agentA)

	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	if repo.ref(ref) != oid1 || e.remote.get(ref) != oid1 || repo.ref(b.tracking(ref)) != oid1 {
		t.Fatalf("local/remote/tracking = %q/%q/%q, want %q", repo.ref(ref), e.remote.get(ref), repo.ref(b.tracking(ref)), oid1)
	}
	if last, _ := log.LastPushed(ctx, ref); last != oid1 {
		t.Fatalf("last pushed = %q, want %q", last, oid1)
	}
	c := e.store.get(oid1)
	if c.message != "gitlet: state" || len(c.parents) != 0 {
		t.Fatalf("commit message/parents = %q/%v", c.message, c.parents)
	}
	if entries := e.store.get(c.tree).entries; len(entries) != 1 || entries[0].Name != "state.json" {
		t.Fatalf("tree = %+v", entries)
	}

	st := sampleState(agentA)
	st.Heartbeat = t0.Add(time.Minute)
	oid2, err := b.WriteState(ctx, st, oid1)
	mustNoErr(t, err)
	pushes := repo.pushLog()
	if len(pushes) != 2 {
		t.Fatalf("%d pushes, want 2", len(pushes))
	}
	want := []gitx.PushUpdate{{Src: oid1, Dst: ref, Expect: gitx.ExpectAbsent}}
	if !reflect.DeepEqual(pushes[0].Updates, want) || !pushes[0].Metadata || pushes[0].Remote != "origin" {
		t.Fatalf("first push = %+v", pushes[0])
	}
	want = []gitx.PushUpdate{{Src: oid2, Dst: ref, Expect: gitx.ExpectOID, OldOID: oid1}}
	if !reflect.DeepEqual(pushes[1].Updates, want) {
		t.Fatalf("second push = %+v", pushes[1].Updates)
	}

	got, gotOID, err := b.ReadOwnState(ctx)
	mustNoErr(t, err)
	st.Format = model.Format
	if gotOID != oid2 || !reflect.DeepEqual(got, st) {
		t.Fatalf("ReadOwnState = %q %+v", gotOID, got)
	}
}

// TestWriteStateDropsBadPaths: a file name git accepts but a document
// cannot carry costs only its footprint entry, never the heartbeat.
func TestWriteStateDropsBadPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	oid1, err := b.WriteState(ctx, stateWithBadPaths(), "")
	mustNoErr(t, err)
	if e.remote.get(StateRef(agentA)) != oid1 {
		t.Fatal("state not pushed")
	}
	got, _, err := b.ReadOwnState(ctx)
	mustNoErr(t, err)
	want := sampleState(agentA)
	want.Format = model.Format
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published state:\n got %+v\nwant %+v", got, want)
	}
}

func TestWriteStateLocalConflict(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	// Another writer moved the local ref: a stale seen oid is refused
	// before anything is pushed.
	if _, err := b.WriteState(ctx, sampleState(agentA), ""); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("err = %v, want local conflict", err)
	}
	if n := len(repo.pushLog()); n != 1 {
		t.Fatalf("%d pushes, want 1", n)
	}
	if repo.ref(StateRef(agentA)) != oid1 {
		t.Fatal("local ref moved")
	}
}

func TestWriteStateAdoptsStaleRemote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	log := &memPushLog{m: map[string]string{}}
	b := e.bus(repo, agentA, withPushLog(log))
	ref := StateRef(agentA)
	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)

	// The remote moved without us knowing (a push whose answer was lost, a
	// restored ledger): the writer adopts it and retries once.
	other := e.store.commit("state.json", []byte(`{"format":1}`), "gitlet: state")
	e.remote.set(ref, other)
	oid2, err := b.WriteState(ctx, sampleState(agentA), oid1)
	mustNoErr(t, err)
	if e.remote.get(ref) != oid2 {
		t.Fatal("remote not updated")
	}
	pushes := repo.pushLog()[1:]
	if len(pushes) != 2 || pushes[1].Updates[0].OldOID != other || pushes[1].Updates[0].Expect != gitx.ExpectOID {
		t.Fatalf("pushes = %+v", pushes)
	}
	fetches := repo.fetchLog()
	if len(fetches) != 1 || !reflect.DeepEqual(fetches[0].Refspecs, []string{"+" + ref + ":" + b.tracking(ref)}) {
		t.Fatalf("fetches = %+v", fetches)
	}
	if last, _ := log.LastPushed(ctx, ref); last != oid2 {
		t.Fatalf("last pushed = %q", last)
	}

	// A remote that lost the ref is recreated create-only.
	e.remote.set(ref, "")
	oid3, err := b.WriteState(ctx, sampleState(agentA), oid2)
	mustNoErr(t, err)
	if e.remote.get(ref) != oid3 {
		t.Fatal("lost ref not recreated")
	}
	if last := repo.pushLog()[len(repo.pushLog())-1]; last.Updates[0].Expect != gitx.ExpectAbsent {
		t.Fatalf("retry = %+v, want create-only", last.Updates[0])
	}

	// A remote already in the wanted state counts as pushed: deleting a ref
	// the remote lost is done after reading it.
	snap := e.store.commit("f.go", []byte("x"), "gitlet: snapshot")
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))
	e.remote.set(WipRef(agentA, itemA), "")
	before := len(repo.pushLog())
	mustNoErr(t, b.DeleteWip(ctx, itemA))
	if n := len(repo.pushLog()) - before; n != 1 {
		t.Fatalf("%d pushes, want 1 (no retry)", n)
	}
	if last, _ := log.LastPushed(ctx, WipRef(agentA, itemA)); last != "" {
		t.Fatalf("last pushed = %q, want forgotten", last)
	}
}

func TestWriteStateIdentityCollision(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	ref := StateRef(agentA)
	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)

	// Another process writes as this agent before each of our pushes.
	e.remote.setBeforePush(func(gitx.PushOptions) {
		e.remote.set(ref, e.store.commit("state.json", []byte(`{"format":1}`), "gitlet: state"))
	})
	oid2, err := b.WriteState(ctx, sampleState(agentA), oid1)
	if !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("err = %v, want ErrIdentityCollision", err)
	}
	if oid2 == "" || repo.ref(ref) != oid2 {
		t.Fatalf("local write did not stand: %q vs %q", oid2, repo.ref(ref))
	}
	if n := len(repo.pushLog()); n != 3 {
		t.Fatalf("%d pushes, want 3 (create, stale, retry)", n)
	}
}

func TestOfflineWriteThenFlush(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	log := &memPushLog{m: map[string]string{}}
	b := e.bus(repo, agentA, withPushLog(log))
	stOID, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)

	e.remote.setDown(true)
	st := sampleState(agentA)
	st.Heartbeat = t0.Add(time.Minute)
	stOID2, err := b.WriteState(ctx, st, stOID)
	if !isNet(err) || stOID2 == "" {
		t.Fatalf("WriteState offline = %q, %v; want the local oid and a network error", stOID2, err)
	}
	obOID, err := b.WriteOutbox(ctx, sampleOutbox(agentA), "")
	if !isNet(err) || obOID == "" {
		t.Fatalf("WriteOutbox offline = %q, %v", obOID, err)
	}
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot")
	if err := b.WriteWip(ctx, itemA, snap); !isNet(err) {
		t.Fatalf("WriteWip offline: %v", err)
	}
	if repo.ref(StateRef(agentA)) != stOID2 || repo.ref(WipRef(agentA, itemA)) != snap {
		t.Fatal("local writes did not stand")
	}
	if e.remote.get(StateRef(agentA)) != stOID {
		t.Fatal("remote changed while down")
	}
	if err := b.Flush(ctx); !isNet(err) {
		t.Fatalf("Flush while down: %v", err)
	}

	e.remote.setDown(false)
	before := len(repo.pushLog())
	mustNoErr(t, b.Flush(ctx))
	pushes := repo.pushLog()[before:]
	if len(pushes) != 2 {
		t.Fatalf("%d pushes, want metadata then wip: %+v", len(pushes), pushes)
	}
	if len(pushes[0].Updates) != 2 || len(pushes[1].Updates) != 1 || pushes[1].Updates[0].Dst != WipRef(agentA, itemA) {
		t.Fatalf("pushes = %+v", pushes)
	}
	for ref, want := range map[string]string{StateRef(agentA): stOID2, OutboxRef(agentA): obOID, WipRef(agentA, itemA): snap} {
		if got := e.remote.get(ref); got != want {
			t.Errorf("remote %s = %q, want %q", ref, got, want)
		}
		if got, _ := log.LastPushed(ctx, ref); got != want {
			t.Errorf("last pushed %s = %q, want %q", ref, got, want)
		}
	}
	before = len(repo.pushLog())
	mustNoErr(t, b.Flush(ctx))
	if n := len(repo.pushLog()) - before; n != 0 {
		t.Fatalf("second Flush pushed %d times", n)
	}
}

func TestFlushRecreatesLostRefs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)

	e.remote.set(StateRef(agentA), "") // reaped by a peer
	_, err = b.Fetch(ctx)
	mustNoErr(t, err)
	if repo.ref(b.tracking(StateRef(agentA))) != "" {
		t.Fatal("poll did not prune the tracking ref")
	}
	mustNoErr(t, b.Flush(ctx))
	if e.remote.get(StateRef(agentA)) != oid1 {
		t.Fatal("lost state not recreated")
	}
	last := repo.pushLog()[len(repo.pushLog())-1]
	if last.Updates[0].Expect != gitx.ExpectAbsent {
		t.Fatalf("recreate = %+v, want create-only", last.Updates[0])
	}
}

func TestFlushDeletesRefsDeletedOffline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot")
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))
	e.remote.setDown(true)
	if err := b.DeleteWip(ctx, itemA); !isNet(err) {
		t.Fatalf("DeleteWip offline: %v", err)
	}
	if repo.ref(WipRef(agentA, itemA)) != "" {
		t.Fatal("local wip not deleted")
	}
	e.remote.setDown(false)
	mustNoErr(t, b.Flush(ctx))
	if e.remote.get(WipRef(agentA, itemA)) != "" {
		t.Fatal("remote wip not deleted by Flush")
	}
	if repo.ref(b.tracking(WipRef(agentA, itemA))) != "" {
		t.Fatal("tracking wip not deleted")
	}
}

// TestWriteWipRefusesUnpushedExcludedFiles: a snapshot whose push would
// publish a denied or oversized file from commits not on the remote yet is
// refused before anything is written or pushed (docs/SPEC.md, section 9.1).
func TestWriteWipRefusesUnpushedExcludedFiles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA, withMaxSnapshotFile(16))
	main := e.remote.get("refs/heads/main")
	repo.setRef("refs/remotes/origin/main", main)

	secret := e.store.commit("config/.env.local", []byte("SECRET=1\n"), "local work", main)
	big := e.store.commit("data.bin", []byte(strings.Repeat("x", 17)), "more local work", secret)
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot", big)
	before := len(repo.pushLog())
	var rej *RejectedError
	err := b.WriteWip(ctx, itemA, snap)
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "config/.env.local, data.bin") {
		t.Fatalf("WriteWip: %v, want a RejectedError naming both files", err)
	}
	if len(repo.pushLog()) != before || repo.ref(WipRef(agentA, itemA)) != "" {
		t.Fatal("the refused snapshot was written or pushed")
	}

	// gitlet's copies of outside branches are never pruned, so they may
	// name commits the remote no longer has and vouch for nothing.
	repo.setRef("refs/gitlet-heads/origin/feature", big)
	if err := b.WriteWip(ctx, itemA, snap); !errors.As(err, &rej) {
		t.Fatalf("WriteWip with the commits only in an outside branch copy: %v, want a RejectedError", err)
	}

	// Once those commits are on the remote, the snapshot is pushed.
	repo.setRef("refs/remotes/origin/feature", big)
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))
	if e.remote.get(WipRef(agentA, itemA)) != snap {
		t.Fatal("snapshot not pushed")
	}

	// Without a remote nothing leaves the machine, so nothing is refused.
	lb := e.bus(e.local(), agentA, withMaxSnapshotFile(16))
	mustNoErr(t, lb.WriteWip(ctx, itemA, snap))
}

// TestFlushSkipsRejectedWip: a snapshot the remote rejected is not pushed
// again by Flush, in this process or another (docs/SPEC.md, section 6.1).
func TestFlushSkipsRejectedWip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	log := &memPushLog{m: map[string]string{}}
	b := e.bus(repo, agentA, withPushLog(log))
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot")
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))

	e.remote.setReject(func(dst, cur string) (string, []string, bool) {
		return "pre-receive hook declined", nil, isWipRef(dst)
	})
	var rej *RejectedError
	snap2 := e.store.commit("f.go", []byte("package f // 2"), "gitlet: snapshot", snap)
	if err := b.WriteWip(ctx, itemA, snap2); !errors.As(err, &rej) {
		t.Fatalf("WriteWip: %v, want a RejectedError", err)
	}
	if got := repo.ref(WipRef(agentA, itemA)); got != snap {
		t.Fatalf("local wip = %q, want it back at the published %q", got, snap)
	}
	snap3 := e.store.commit("g.go", []byte("package g"), "gitlet: snapshot")
	if err := b.WriteWip(ctx, itemB, snap3); !errors.As(err, &rej) {
		t.Fatalf("WriteWip of a new item: %v, want a RejectedError", err)
	}
	if got := repo.ref(WipRef(agentA, itemB)); got != "" {
		t.Fatalf("local wip of a never published item = %q, want none", got)
	}

	before := len(repo.pushLog())
	mustNoErr(t, b.Flush(ctx))
	mustNoErr(t, e.bus(repo, agentA, withPushLog(log)).Flush(ctx))
	if pushes := repo.pushLog()[before:]; len(pushes) != 0 {
		t.Fatalf("Flush pushed the rejected snapshots again: %+v", pushes)
	}
}

// TestFlushSkipsRejectedDocs: a state write or a deletion the remote
// rejected is not retried by every Flush; the next write is pushed as usual.
func TestFlushSkipsRejectedDocs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot")
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))

	// Like receive.denyNonFastForwards and receive.denyDeletes: every state
	// update and every deletion is refused.
	e.remote.setReject(func(dst, cur string) (string, []string, bool) {
		return "denied", nil, dst == StateRef(agentA) || isWipRef(dst)
	})
	var rej *RejectedError
	oid, err := b.WriteState(ctx, sampleState(agentA), "")
	if !errors.As(err, &rej) || oid == "" || repo.ref(StateRef(agentA)) != oid {
		t.Fatalf("WriteState = %q, %v; want the local write and a RejectedError", oid, err)
	}
	if err := b.DeleteWip(ctx, itemA); !errors.As(err, &rej) {
		t.Fatalf("DeleteWip: %v, want a RejectedError", err)
	}
	before := len(repo.pushLog())
	for range 3 {
		mustNoErr(t, b.Flush(ctx))
	}
	if pushes := repo.pushLog()[before:]; len(pushes) != 0 {
		t.Fatalf("Flush retried the rejected updates: %+v", pushes)
	}

	e.remote.setReject(nil)
	st := sampleState(agentA)
	st.Heartbeat = t0.Add(time.Minute)
	oid2, err := b.WriteState(ctx, st, oid)
	mustNoErr(t, err)
	if e.remote.get(StateRef(agentA)) != oid2 {
		t.Fatal("the next state write was not published")
	}
}

func TestObserverMode(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	e.remote.setDenied(true)

	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	if !errors.Is(err, ErrObserver) || oid1 == "" {
		t.Fatalf("WriteState = %q, %v; want the local oid and ErrObserver", oid1, err)
	}
	if n := len(repo.pushLog()); n != 1 {
		t.Fatalf("%d pushes, want 1", n)
	}
	// Read-only now: nothing is pushed until the re-probe.
	if _, err := b.WriteState(ctx, sampleState(agentA), oid1); !errors.Is(err, ErrObserver) {
		t.Fatalf("second write: %v", err)
	}
	if _, _, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), ""); !errors.Is(err, ErrObserver) {
		t.Fatalf("CreateClaim: %v", err)
	}
	if err := b.Flush(ctx); !errors.Is(err, ErrObserver) {
		t.Fatalf("Flush: %v", err)
	}
	if n := len(repo.pushLog()); n != 1 {
		t.Fatalf("%d pushes in observer mode, want 1", n)
	}
	_, err = b.Fetch(ctx) // reading still works
	mustNoErr(t, err)

	// After an hour the next push is the re-probe; it succeeds now.
	e.remote.setDenied(false)
	e.clock.Advance(observerRetry)
	mustNoErr(t, b.Flush(ctx))
	if e.remote.get(StateRef(agentA)) == "" {
		t.Fatal("state not published after re-probe")
	}
	cur := repo.ref(StateRef(agentA))
	if _, err := b.WriteState(ctx, sampleState(agentA), cur); err != nil {
		t.Fatalf("write after leaving observer mode: %v", err)
	}
}

func TestWriteWipOwnPushAndRejection(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	snap := e.store.commit("f.go", []byte("package f"), "gitlet: snapshot")
	mustNoErr(t, b.WriteWip(ctx, itemA, snap))
	push := repo.pushLog()[0]
	if len(push.Updates) != 1 || push.Updates[0].Dst != WipRef(agentA, itemA) || !push.Metadata {
		t.Fatalf("wip push = %+v", push)
	}
	if repo.ref(b.tracking(WipRef(agentA, itemA))) != snap {
		t.Fatal("tracking wip not updated")
	}

	e.remote.setReject(func(dst, cur string) (string, []string, bool) {
		if isWipRef(dst) {
			return "pre-receive hook declined", []string{"GH013: secret detected in config/.env.local"}, true
		}
		return "", nil, false
	})
	snap2 := e.store.commit("f.go", []byte("package f // 2"), "gitlet: snapshot", snap)
	err := b.WriteWip(ctx, itemA, snap2)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Kind != gitx.RejectRemote || rej.Ref != WipRef(agentA, itemA) ||
		len(rej.Remote) != 1 || !strings.Contains(rej.Remote[0], "GH013") {
		t.Fatalf("err = %v (%+v), want a RejectedError with the remote lines", err, rej)
	}
	// A rejected snapshot does not block the heartbeat.
	_, err = b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)

	for _, bad := range []struct {
		item   model.ItemID
		commit string
	}{{"bad", snap}, {itemA, "HEAD"}} {
		if err := b.WriteWip(ctx, bad.item, bad.commit); err == nil {
			t.Errorf("WriteWip(%q, %q) succeeded", bad.item, bad.commit)
		}
	}
}

func TestDeleteOwn(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	log := &memPushLog{m: map[string]string{}}
	b := e.bus(repo, agentA, withPushLog(log))
	_, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	_, err = b.WriteOutbox(ctx, sampleOutbox(agentA), "")
	mustNoErr(t, err)
	mustNoErr(t, b.WriteWip(ctx, itemA, e.store.commit("f.go", []byte("x"), "gitlet: snapshot")))
	other := e.bus(e.clone(), agentB)
	_, err = other.WriteState(ctx, sampleState(agentB), "")
	mustNoErr(t, err)

	before := len(repo.pushLog())
	mustNoErr(t, b.DeleteOwn(ctx))
	if pushes := repo.pushLog()[before:]; len(pushes) != 1 || len(pushes[0].Updates) != 3 {
		t.Fatalf("pushes = %+v, want one push of three deletions", pushes)
	}
	for _, ref := range []string{StateRef(agentA), OutboxRef(agentA), WipRef(agentA, itemA)} {
		if repo.ref(ref) != "" || e.remote.get(ref) != "" || repo.ref(b.tracking(ref)) != "" {
			t.Errorf("%s survived", ref)
		}
		if last, _ := log.LastPushed(ctx, ref); last != "" {
			t.Errorf("last pushed %s = %q", ref, last)
		}
	}
	if e.remote.get(StateRef(agentB)) == "" {
		t.Fatal("deleted another agent's state")
	}
	mustNoErr(t, b.DeleteOwn(ctx)) // nothing left: no-op
}

func TestReadOwn(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	st, oid, err := b.ReadOwnState(ctx)
	mustNoErr(t, err)
	if oid != "" || !reflect.DeepEqual(st, model.AgentState{}) {
		t.Fatalf("missing state = %q %+v", oid, st)
	}
	ob, oid, err := b.ReadOwnOutbox(ctx)
	mustNoErr(t, err)
	if oid != "" || ob.Agent != "" {
		t.Fatalf("missing outbox = %q %+v", oid, ob)
	}
	obOID, err := b.WriteOutbox(ctx, sampleOutbox(agentA), "")
	mustNoErr(t, err)
	ob, oid, err = b.ReadOwnOutbox(ctx)
	mustNoErr(t, err)
	if oid != obOID || len(ob.Messages) != 1 {
		t.Fatalf("outbox = %q %+v", oid, ob)
	}

	// A broken own document still reports its oid, so it can be replaced.
	bad := e.store.commit("state.json", []byte(`{"format":1,"agent":{"id":"someone-else"}}`), "gitlet: state")
	repo.setRef(StateRef(agentA), bad)
	if _, oid, err := b.ReadOwnState(ctx); err == nil || oid != bad {
		t.Fatalf("broken state = %q, %v", oid, err)
	}
	newer := e.store.commit("state.json", []byte(`{"format":7}`), "gitlet: state")
	repo.setRef(StateRef(agentA), newer)
	if _, _, err := b.ReadOwnState(ctx); !errors.Is(err, errNewerFormat) {
		t.Fatalf("newer state: %v", err)
	}
}

func TestOwnRefsWithoutRemote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.local()
	b := e.bus(repo, agentA)
	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	if repo.ref(StateRef(agentA)) != oid1 {
		t.Fatal("local state not written")
	}
	mustNoErr(t, b.WriteWip(ctx, itemA, e.store.commit("f.go", []byte("x"), "gitlet: snapshot")))
	mustNoErr(t, b.Flush(ctx))
	res, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if res.Fingerprint == "" || res.Reused {
		t.Fatalf("fetch = %+v", res)
	}
	mustNoErr(t, b.DeleteOwn(ctx))
	if repo.ref(StateRef(agentA)) != "" || repo.ref(WipRef(agentA, itemA)) != "" {
		t.Fatal("own refs not deleted")
	}
	if len(repo.pushLog()) != 0 || len(repo.fetchLog()) != 0 {
		t.Fatalf("contacted a remote: %d pushes, %d fetches", len(repo.pushLog()), len(repo.fetchLog()))
	}
}

func TestConcurrentOwnWrites(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	done := make(chan error)
	for i := 0; i < 8; i++ {
		go func() {
			snap := e.store.commit("f.go", []byte("x"+itoa(i)), "gitlet: snapshot")
			done <- b.WriteWip(ctx, model.ItemID("aaaaaaaaaa"+itoa(10+i)), snap)
		}()
	}
	for i := 0; i < 8; i++ {
		mustNoErr(t, <-done)
	}
	if n := len(e.remote.list(WipPrefix)); n != 8 {
		t.Fatalf("%d wips on the remote, want 8", n)
	}
}
