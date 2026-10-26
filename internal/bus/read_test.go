package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

func TestFetchRefspecsAndPrune(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a := e.bus(e.clone(), agentA)
	_, err := a.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	_, _, err = a.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	mustNoErr(t, err)

	repo := e.clone()
	b := e.bus(repo, agentB)
	res, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if res.Reused || !res.At.Equal(t0) || res.Fingerprint == "" {
		t.Fatalf("fetch = %+v", res)
	}
	f := repo.fetchLog()[0]
	want := []string{
		"+refs/gitlet/agents/*:refs/gitlet-remote/origin/agents/*",
		"+refs/gitlet/claims/*:refs/gitlet-remote/origin/claims/*",
		"+refs/heads/main:refs/gitlet-main/origin",
	}
	if !reflect.DeepEqual(f.Refspecs, want) || !f.Prune || !f.Metadata || f.Remote != "origin" {
		t.Fatalf("fetch options = %+v", f)
	}
	for _, ref := range []string{StateRef(agentA), ClaimRef(keyA)} {
		if repo.ref(b.tracking(ref)) != e.remote.get(ref) {
			t.Errorf("tracking %s not fetched", ref)
		}
	}
	if repo.ref(b.MainRef()) != e.remote.get("refs/heads/main") || repo.ref("refs/remotes/origin/main") != "" {
		t.Fatal("main must go to gitlet's own ref only")
	}

	mustNoErr(t, a.DeleteOwn(ctx))
	e.clock.Advance(time.Minute)
	res2, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if repo.ref(b.tracking(StateRef(agentA))) != "" {
		t.Fatal("deleted state not pruned")
	}
	if res2.Fingerprint == res.Fingerprint {
		t.Fatal("fingerprint did not change")
	}
}

func TestFetchWithoutMainBranch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.remote.set("refs/heads/main", "")
	repo := e.clone()
	b := e.bus(repo, agentB)
	_, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if f := repo.fetchLog(); len(f) != 2 || len(f[1].Refspecs) != 2 {
		t.Fatalf("fetches = %+v, want a retry without main", f)
	}
}

func TestFetchReuse(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	lockDir := t.TempDir()
	// Two processes of one clone (a server and a CLI) share the lock dir.
	b1 := e.bus(repo, agentA, withLockDir(lockDir))
	b2 := e.bus(repo, agentB, withLockDir(lockDir))

	r1, err := b1.Fetch(ctx)
	mustNoErr(t, err)
	e.clock.Advance(3 * time.Second)
	r2, err := b2.Fetch(ctx)
	mustNoErr(t, err)
	if !r2.Reused || !r2.At.Equal(r1.At) || r2.Fingerprint != r1.Fingerprint {
		t.Fatalf("second fetch = %+v, want reuse of %+v", r2, r1)
	}
	if n := len(repo.fetchLog()); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(lockDir, "fetch.lock")); err != nil {
		t.Fatalf("fetch lock: %v", err)
	}

	e.clock.Advance(3 * time.Second) // 6 s after the fetch
	r3, err := b2.Fetch(ctx)
	mustNoErr(t, err)
	if r3.Reused || !r3.At.Equal(t0.Add(6*time.Second)) || len(repo.fetchLog()) != 2 {
		t.Fatalf("third fetch = %+v after %d fetches", r3, len(repo.fetchLog()))
	}

	// A stamp from another remote, or from the future, is not reused.
	other := New(repo, agentA, Options{Remote: "fork", LockDir: lockDir, Now: e.clock.Now})
	if _, ok := other.freshStamp(); ok {
		t.Fatal("reused another remote's fetch")
	}
	e.clock.Advance(-time.Minute)
	if _, ok := b1.freshStamp(); ok {
		t.Fatal("reused a fetch from the future")
	}
	noReuse := e.bus(repo, agentA, withLockDir(lockDir), func(o *Options) { o.FetchReuse = -1 })
	e.clock.Advance(time.Minute)
	if r, err := noReuse.Fetch(ctx); err != nil || r.Reused {
		t.Fatalf("FetchReuse<0 = %+v, %v", r, err)
	}
}

func TestFetchUnreachable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	lockDir := t.TempDir()
	b := e.bus(e.clone(), agentA, withLockDir(lockDir))
	e.remote.setDown(true)
	if _, err := b.Fetch(ctx); !isNet(err) {
		t.Fatalf("err = %v, want a network error", err)
	}
	if _, err := os.Stat(filepath.Join(lockDir, stampFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed fetch left a stamp")
	}
}

func TestFingerprintTracksChanges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentB, func(o *Options) { o.FetchReuse = -1 })
	r1, err := b.Fetch(ctx)
	mustNoErr(t, err)
	r2, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if r1.Fingerprint != r2.Fingerprint {
		t.Fatal("fingerprint changed without changes")
	}
	a := e.bus(e.clone(), agentA)
	_, err = a.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	r3, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if r3.Fingerprint == r2.Fingerprint {
		t.Fatal("fingerprint missed a peer's state")
	}
	// Own documents written by another process of this worktree count too.
	_, err = b.WriteOutbox(ctx, sampleOutbox(agentB), "")
	if err != nil {
		t.Fatal(err)
	}
	repo.setRef(OutboxRef(agentB), e.store.commit("outbox.json", []byte(`{"format":1}`), "gitlet: outbox"))
	r4, err := b.Fetch(ctx)
	mustNoErr(t, err)
	if r4.Fingerprint == r3.Fingerprint {
		t.Fatal("fingerprint missed an own document")
	}
}

func TestLoad(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a := e.bus(e.clone(), agentA)
	stOID, err := a.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	obOID, err := a.WriteOutbox(ctx, sampleOutbox(agentA), "")
	mustNoErr(t, err)
	_, claim, err := a.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	mustNoErr(t, err)

	repo := e.clone()
	b := e.bus(repo, agentB)
	_, err = b.WriteState(ctx, sampleState(agentB), "")
	mustNoErr(t, err)
	_, err = b.Fetch(ctx)
	mustNoErr(t, err)
	st, err := b.Load(ctx)
	mustNoErr(t, err)
	if len(st.Skipped) != 0 {
		t.Fatalf("skipped = %v", st.Skipped)
	}
	want := sampleState(agentA)
	want.Format = model.Format
	if !reflect.DeepEqual(st.Agents[agentA], want) || st.Agents[agentB].Agent.ID != agentB {
		t.Fatalf("agents = %+v", st.Agents)
	}
	if len(st.Outboxes[agentA].Messages) != 1 || !reflect.DeepEqual(st.Claims[keyA], claim) {
		t.Fatalf("outboxes/claims = %+v / %+v", st.Outboxes, st.Claims)
	}
	if st.Refs[StateRef(agentA)] != stOID || st.Refs[OutboxRef(agentA)] != obOID || st.Refs[ClaimRef(keyA)] != claim.OID {
		t.Fatalf("refs = %v", st.Refs)
	}
	if st.Main != e.remote.get("refs/heads/main") {
		t.Fatalf("main = %q", st.Main)
	}
	if n := repo.readerCount(); n != 1 {
		t.Fatalf("%d readers opened, want 1", n)
	}

	// Own documents come from the local refs, even when they are newer
	// than what the remote has.
	e.remote.setDown(true)
	own := sampleState(agentB)
	own.Heartbeat = t0.Add(time.Hour)
	ownOID, err := b.WriteState(ctx, own, repo.ref(StateRef(agentB)))
	if !isNet(err) {
		t.Fatalf("offline write: %v", err)
	}
	st, err = b.Load(ctx)
	mustNoErr(t, err)
	if !st.Agents[agentB].Heartbeat.Equal(own.Heartbeat) || st.Refs[StateRef(agentB)] != ownOID {
		t.Fatalf("own state = %+v at %q", st.Agents[agentB], st.Refs[StateRef(agentB)])
	}
	if _, ok := st.Outboxes[agentB]; ok {
		t.Fatal("own outbox appeared from nowhere")
	}
}

func TestLoadSkipsInvalidDocuments(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	put := func(ref, file, data string) {
		e.remote.set(ref, e.store.commit(file, []byte(data), "gitlet: test"))
	}
	good := e.bus(e.clone(), agentA)
	_, err := good.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	put(StateRef("newer-agent-aaaaaa"), "state.json", `{"format":2,"agent":"different shape"}`)
	put(StateRef("big-agent-bbbbbb"), "state.json", `{"format":1,"pad":"`+strings.Repeat("x", MaxReadDoc)+`"}`)
	put(StateRef("liar-agent-cccccc"), "state.json", `{"format":1,"agent":{"id":"`+string(agentA)+`"},"items":[]}`)
	put(OutboxRef("path-agent-dddddd"), "outbox.json", `{"format":1,"agent":"path-agent-dddddd","needs":[{"id":"aaaaaaaaaaaa","key":"`+keyA+`","item":"../../etc"}]}`)
	put(AgentsPrefix+"Bad_Agent/state", "state.json", `{"format":1}`)
	put(AgentsPrefix+"odd-agent-eeeeee/notes", "notes.json", `{"format":1}`)
	put(StateRef("empty-agent-ffffff"), "README", `hello`)
	put(ClaimRef("nothex"), "claim.json", `{"format":1}`)
	put(ClaimRef(keyB), "claim.json", `{"format":1,"key":"`+keyA+`","owner":"`+string(agentA)+`"}`)
	put(ClaimRef(keyC), "claim.json", `not json`)

	repo := e.clone()
	b := e.bus(repo, agentB)
	_, err = b.Fetch(ctx)
	mustNoErr(t, err)
	st, err := b.Load(ctx)
	mustNoErr(t, err)
	if _, ok := st.Agents[agentA]; !ok || len(st.Agents) != 1 || len(st.Outboxes) != 0 || len(st.Claims) != 0 {
		t.Fatalf("loaded %d agents, %d outboxes, %d claims", len(st.Agents), len(st.Outboxes), len(st.Claims))
	}
	wantSkipped := []string{
		"refs/gitlet/agents/Bad_Agent/state: invalid agent id",
		"refs/gitlet/agents/big-agent-bbbbbb/state: too large",
		"refs/gitlet/agents/empty-agent-ffffff/state: commit",
		"refs/gitlet/agents/liar-agent-cccccc/state: agent id",
		"refs/gitlet/agents/newer-agent-aaaaaa/state: newer format",
		"refs/gitlet/agents/odd-agent-eeeeee/notes: unknown agents ref",
		"refs/gitlet/agents/path-agent-dddddd/outbox: needs[0]: invalid item id",
		"refs/gitlet/claims/4455667788990011: key",
		"refs/gitlet/claims/ffeeddccbbaa9988: invalid JSON",
		"refs/gitlet/claims/nothex: invalid symbol key",
	}
	if len(st.Skipped) != len(wantSkipped) {
		t.Fatalf("skipped = %q", st.Skipped)
	}
	for i, w := range wantSkipped {
		if !strings.HasPrefix(st.Skipped[i], w) {
			t.Errorf("skipped[%d] = %q, want prefix %q", i, st.Skipped[i], w)
		}
	}
	// Skipped documents still count for lease observation.
	if st.Refs[StateRef("newer-agent-aaaaaa")] == "" {
		t.Fatal("refs lack a skipped document")
	}
}

func TestLoadWithoutRemote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.local()
	a, b := e.bus(repo, agentA), e.bus(repo, agentB)
	_, err := a.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	_, _, err = b.CreateClaim(ctx, sampleClaim(keyA, agentB, itemB), "")
	mustNoErr(t, err)
	st, err := b.Load(ctx)
	mustNoErr(t, err)
	if st.Agents[agentA].Agent.ID != agentA || st.Claims[keyA].Claim.Owner != agentB || st.Main != repo.ref("refs/heads/main") {
		t.Fatalf("state = %+v", st)
	}
}

func TestFetchWipOnDemand(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a := e.bus(e.clone(), agentA)
	snap := e.store.commit("f.go", []byte("x"), "gitlet: snapshot")
	mustNoErr(t, a.WriteWip(ctx, itemA, snap))

	repo := e.clone()
	b := e.bus(repo, agentB)
	got, err := b.FetchWip(ctx, agentA, itemA, snap)
	mustNoErr(t, err)
	if got != snap || repo.ref(b.tracking(WipRef(agentA, itemA))) != snap {
		t.Fatalf("FetchWip = %q", got)
	}
	want := []string{"+" + WipRef(agentA, itemA) + ":refs/gitlet-remote/origin/wip/" + string(agentA) + "/" + string(itemA)}
	if f := repo.fetchLog(); len(f) != 1 || !reflect.DeepEqual(f[0].Refspecs, want) {
		t.Fatalf("fetches = %+v", f)
	}
	got, err = b.FetchWip(ctx, agentA, itemA, snap)
	mustNoErr(t, err)
	if got != snap || len(repo.fetchLog()) != 1 {
		t.Fatal("fetched a snapshot that was already local")
	}
	// A newer snapshot than the state said is returned as is.
	snap2 := e.store.commit("f.go", []byte("y"), "gitlet: snapshot", snap)
	mustNoErr(t, a.WriteWip(ctx, itemA, snap2))
	if got, err = b.FetchWip(ctx, agentA, itemA, oid("old")); err != nil || got != snap2 {
		t.Fatalf("FetchWip = %q, %v", got, err)
	}
	if _, err := b.FetchWip(ctx, agentA, itemB, snap); !errors.Is(err, gitx.ErrNotFound) {
		t.Fatalf("missing wip: %v", err)
	}
	if _, err := b.FetchWip(ctx, agentA, itemA, "HEAD"); err == nil {
		t.Fatal("invalid want accepted")
	}

	// A peer in another worktree of this clone has it without fetching.
	local := e.bus(repo, agentA)
	snap3 := e.store.commit("f.go", []byte("z"), "gitlet: snapshot")
	e.remote.setDown(true)
	_ = local.WriteWip(ctx, itemB, snap3)
	if got, err = b.FetchWip(ctx, agentA, itemB, snap3); err != nil || got != snap3 {
		t.Fatalf("same-clone wip = %q, %v", got, err)
	}
}

func TestPruneWips(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentB)
	snap := e.store.commit("f.go", []byte("x"), "gitlet: snapshot")
	repo.setRef(b.tracking(WipRef(agentA, itemA)), snap)
	repo.setRef(b.tracking(WipRef(agentA, itemB)), snap)
	mustNoErr(t, b.WriteWip(ctx, itemB, snap)) // own wip: never pruned
	mustNoErr(t, b.PruneWips(ctx, map[string]bool{WipRef(agentA, itemA): true}))
	if repo.ref(b.tracking(WipRef(agentA, itemA))) == "" || repo.ref(b.tracking(WipRef(agentA, itemB))) != "" {
		t.Fatal("pruned the wrong peer wip")
	}
	if repo.ref(b.tracking(WipRef(agentB, itemB))) == "" || repo.ref(WipRef(agentB, itemB)) == "" {
		t.Fatal("pruned an own wip")
	}

	// Without a remote the wip refs are the peers' own: nothing is pruned.
	lrepo := e.local()
	lrepo.setRef(WipRef(agentA, itemA), snap)
	mustNoErr(t, e.bus(lrepo, agentB).PruneWips(ctx, nil))
	if lrepo.ref(WipRef(agentA, itemA)) == "" {
		t.Fatal("pruned a peer's wip in local mode")
	}
}

func TestFetchOutside(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	branch := func(name string, age time.Duration) {
		e.clock.Advance(-age)
		e.remote.set("refs/heads/"+name, e.store.commit("f", []byte(name), name))
		e.clock.Advance(age)
	}
	branch("fresh", time.Hour)
	branch("feature/x", 2*time.Hour)
	branch("week-old", 8*24*time.Hour)
	branch("newest", time.Minute)
	e.remote.set("refs/heads/main", e.store.commit("f", []byte("main"), "main"))

	repo := e.clone()
	b := e.bus(repo, agentB)
	got, err := b.FetchOutside(ctx, 168*time.Hour, 2)
	mustNoErr(t, err)
	var names []string
	for _, ri := range got {
		names = append(names, ri.Name)
	}
	if want := []string{"refs/heads/newest", "refs/heads/fresh"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("branches = %v, want %v", names, want)
	}
	f := repo.fetchLog()[0]
	if !reflect.DeepEqual(f.Refspecs, []string{"+refs/heads/*:refs/gitlet-heads/origin/*"}) || f.Prune {
		t.Fatalf("fetch = %+v", f)
	}
	got, err = b.FetchOutside(ctx, 168*time.Hour, 0)
	mustNoErr(t, err)
	if len(got) != 3 {
		t.Fatalf("%d branches, want 3", len(got))
	}
	if got, err := e.bus(e.local(), agentB).FetchOutside(ctx, time.Hour, 5); err != nil || got != nil {
		t.Fatalf("local FetchOutside = %v, %v", got, err)
	}
}

func TestRefreshMain(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	main := e.store.commit("f", []byte("new main"), "next", e.remote.get("refs/heads/main"))
	e.remote.set("refs/heads/main", main)
	got, err := b.RefreshMain(ctx)
	mustNoErr(t, err)
	if got != main || repo.ref("refs/remotes/origin/main") != main || repo.ref("refs/gitlet-main/origin") != main {
		t.Fatalf("RefreshMain = %q", got)
	}
	l := e.local()
	if got, err := e.bus(l, agentA).RefreshMain(ctx); err != nil || got != main {
		t.Fatalf("local RefreshMain = %q, %v", got, err)
	}
	e.remote.set("refs/heads/main", "")
	if _, err := b.RefreshMain(ctx); !errors.Is(err, gitx.ErrNotFound) {
		t.Fatalf("missing main: %v", err)
	}
}

func TestPushBranch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	base := e.remote.get("refs/heads/main")
	c1 := e.store.commit("f", []byte("1"), "one", base)
	r, err := b.PushBranch(ctx, c1, "gitlet/work-1234", "")
	mustNoErr(t, err)
	if !r.OK || e.remote.get("refs/heads/gitlet/work-1234") != c1 {
		t.Fatalf("first push = %+v", r)
	}
	if p := repo.pushLog()[0]; p.Metadata || p.Updates[0].Expect != gitx.ExpectFastForward {
		t.Fatalf("branch push must run hooks and fast-forward: %+v", p)
	}

	// Rebased: not a fast-forward, so the lease on what we pushed applies.
	c2 := e.store.commit("f", []byte("2"), "two", base)
	r, err = b.PushBranch(ctx, c2, "gitlet/work-1234", c1)
	mustNoErr(t, err)
	if !r.OK || e.remote.get("refs/heads/gitlet/work-1234") != c2 {
		t.Fatalf("lease push = %+v", r)
	}
	last := repo.pushLog()[len(repo.pushLog())-1]
	if last.Updates[0].Expect != gitx.ExpectOID || last.Updates[0].OldOID != c1 {
		t.Fatalf("lease push options = %+v", last.Updates[0])
	}

	// Someone else pushed: refused.
	c3 := e.store.commit("f", []byte("3"), "three", base)
	_, err = b.PushBranch(ctx, c3, "gitlet/work-1234", c1)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Kind != gitx.RejectStale {
		t.Fatalf("err = %v, want a stale rejection", err)
	}
	_, err = b.PushBranch(ctx, c3, "gitlet/work-1234", "")
	if !errors.As(err, &rej) || rej.Kind != gitx.RejectNonFF {
		t.Fatalf("err = %v, want a non-fast-forward rejection", err)
	}
	if _, err := b.PushBranch(ctx, c3, "-delete", ""); err == nil {
		t.Fatal("invalid branch accepted")
	}
	if _, err := e.bus(e.local(), agentA).PushBranch(ctx, c3, "x", ""); !errors.Is(err, errNoRemote) {
		t.Fatalf("local PushBranch: %v", err)
	}
}

func TestReap(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	gone := e.bus(e.clone(), agentA)
	stOID, err := gone.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	obOID, err := gone.WriteOutbox(ctx, sampleOutbox(agentA), "")
	mustNoErr(t, err)

	repo := e.clone()
	b := e.bus(repo, agentB)
	_, err = b.Fetch(ctx)
	mustNoErr(t, err)
	// The outbox changed after we judged it lapsed: it is left alone.
	ob := sampleOutbox(agentA)
	ob.Accepts = nil
	_, err = gone.WriteOutbox(ctx, ob, obOID)
	mustNoErr(t, err)
	mustNoErr(t, b.Reap(ctx, map[string]string{StateRef(agentA): stOID, OutboxRef(agentA): obOID}))
	if e.remote.get(StateRef(agentA)) != "" || repo.ref(b.tracking(StateRef(agentA))) != "" {
		t.Fatal("lapsed state not reaped")
	}
	if e.remote.get(OutboxRef(agentA)) == "" {
		t.Fatal("changed outbox reaped")
	}
	if p := repo.pushLog(); len(p) != 1 || len(p[0].Updates) != 2 {
		t.Fatalf("pushes = %+v, want one push", p)
	}
	if err := b.Reap(ctx, map[string]string{"refs/heads/main": stOID}); err == nil {
		t.Fatal("reaped a ref outside refs/gitlet/")
	}

	// Without a remote the shared refs are deleted in place, with the same lease.
	lrepo := e.local()
	la, lb := e.bus(lrepo, agentA), e.bus(lrepo, agentB)
	stOID, err = la.WriteState(ctx, sampleState(agentA), "")
	mustNoErr(t, err)
	obOID, err = la.WriteOutbox(ctx, sampleOutbox(agentA), "")
	mustNoErr(t, err)
	mustNoErr(t, lb.Reap(ctx, map[string]string{StateRef(agentA): stOID, OutboxRef(agentA): oid("judged lapsed")}))
	if lrepo.ref(StateRef(agentA)) != "" || lrepo.ref(OutboxRef(agentA)) != obOID {
		t.Fatal("local reap ignored a lease")
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	repo := e.clone()
	b := e.bus(repo, agentA)
	res, err := b.Probe(ctx)
	mustNoErr(t, err)
	if !res.Create || !res.NonFastForward || !res.Delete {
		t.Fatalf("probe = %+v", res)
	}
	if e.remote.get(ProbePrefix+string(agentA)) != "" {
		t.Fatal("probe ref left behind")
	}
	p := repo.pushLog()
	if len(p) != 3 || p[0].Updates[0].Expect != gitx.ExpectAbsent || p[1].Updates[0].Expect != gitx.ExpectOID || p[2].Updates[0].Src != "" {
		t.Fatalf("probe pushes = %+v", p)
	}

	// A protected namespace refuses rewrites.
	e.remote.setReject(func(dst, cur string) (string, []string, bool) {
		if cur != "" {
			return "protected ref", []string{"cannot rewrite " + dst}, true
		}
		return "", nil, false
	})
	res, err = b.Probe(ctx)
	mustNoErr(t, err)
	if !res.Create || res.NonFastForward || res.Delete || !strings.Contains(res.Detail, "cannot rewrite") {
		t.Fatalf("probe = %+v", res)
	}
	// A leftover probe ref is removed first once the remote allows it.
	e.remote.setReject(nil)
	res, err = b.Probe(ctx)
	mustNoErr(t, err)
	if !res.Create || !res.NonFastForward || !res.Delete || !strings.Contains(res.Detail, "remove leftover: ok") {
		t.Fatalf("probe = %+v", res)
	}

	e.remote.setDenied(true)
	res, err = b.Probe(ctx)
	mustNoErr(t, err)
	if res.Create || !strings.Contains(res.Detail, "permission denied") {
		t.Fatalf("probe = %+v", res)
	}
	if _, err := b.WriteState(ctx, sampleState(agentA), ""); !errors.Is(err, ErrObserver) {
		t.Fatalf("after a denied probe: %v", err)
	}
	e.remote.setDenied(false)
	if res, err = b.Probe(ctx); err != nil || !res.Create {
		t.Fatalf("probe = %+v, %v", res, err)
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatalf("a successful probe must leave observer mode: %v", err)
	}

	e.remote.setDown(true)
	if _, err := b.Probe(ctx); !isNet(err) {
		t.Fatalf("probe while down: %v", err)
	}
	res, err = e.bus(e.local(), agentA).Probe(ctx)
	if err != nil || !res.Create || !res.NonFastForward || !res.Delete {
		t.Fatalf("local probe = %+v, %v", res, err)
	}
}
