package bus

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// These tests drive the bus through real git: a bare repository is the
// remote and each clone is one machine.

// must fails the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// git runs a setup command and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

type gitEnv struct {
	t      *testing.T
	dir    string
	remote string // the bare repository
	seed   string // a clone used to move main
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	g := &gitEnv{t: t, dir: dir, remote: filepath.Join(dir, "remote.git"), seed: filepath.Join(dir, "seed")}
	git(t, dir, "init", "-q", "--bare", g.remote)
	git(t, dir, "init", "-q", g.seed)
	g.commit("README.md", "hello\n", "initial")
	git(t, g.seed, "remote", "add", "origin", g.remote)
	git(t, g.seed, "push", "-q", "origin", "main")
	return g
}

// commit commits a file in the seed clone and pushes main.
func (g *gitEnv) commit(file, content, message string) string {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.seed, file), []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
	git(g.t, g.seed, "add", file)
	git(g.t, g.seed, "commit", "-q", "-m", message)
	return git(g.t, g.seed, "rev-parse", "HEAD")
}

// clone makes a clone of the remote and opens it with gitx.
func (g *gitEnv) clone(name string) *gitx.Repo {
	g.t.Helper()
	path := filepath.Join(g.dir, name)
	git(g.t, g.dir, "clone", "-q", g.remote, path)
	repo, err := gitx.Open(context.Background(), path)
	must(g.t, err)
	return repo.Background()
}

func (g *gitEnv) bus(repo *gitx.Repo, me model.AgentID) *Bus {
	return New(repo, me, Options{
		Remote:     "origin",
		Main:       "main",
		LockDir:    filepath.Join(repo.CommonDir, "gitlet"),
		PushLog:    &memPushLog{m: map[string]string{}},
		FetchReuse: -1, // every Fetch in these tests really fetches
	})
}

// remoteRef reads a ref of the bare remote ("" when missing).
func (g *gitEnv) remoteRef(ref string) string {
	g.t.Helper()
	out, err := exec.Command("git", "-C", g.remote, "for-each-ref", "--format=%(objectname)", ref).Output()
	if err != nil {
		g.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestGitClaimRace(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	buses := []*Bus{g.bus(g.clone("a"), agentA), g.bus(g.clone("b"), agentB)}
	agents := []model.AgentID{agentA, agentB}
	items := []model.ItemID{itemA, itemB}
	won := make([]bool, 2)
	views := make([]ClaimView, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range buses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won[i], views[i], errs[i] = buses[i].CreateClaim(ctx, sampleClaim(keyA, agents[i], items[i]), "")
		}()
	}
	wg.Wait()
	must(t, errs[0])
	must(t, errs[1])
	if won[0] == won[1] {
		t.Fatalf("won = %v, want exactly one winner", won)
	}
	w, l := 0, 1
	if won[1] {
		w, l = 1, 0
	}
	if g.remoteRef(ClaimRef(keyA)) != views[w].OID {
		t.Fatalf("remote claim = %q, winner %q", g.remoteRef(ClaimRef(keyA)), views[w].OID)
	}
	if views[l].OID != views[w].OID || views[l].Claim.Owner != agents[w] {
		t.Fatalf("loser view = %+v, want the winner's claim", views[l])
	}

	// The loser's stale update is refused; the holder renews in a batch.
	if _, err := buses[l].UpdateClaim(ctx, sampleClaim(keyA, agents[l], items[l]), oid("stale"), ""); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale update: %v", err)
	}
	c := views[w].Claim
	c.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	out, err := buses[w].UpdateClaims(ctx, []ClaimUpdate{{Claim: c, SeenOID: views[w].OID}})
	must(t, err)
	if out[0].Err != nil || g.remoteRef(ClaimRef(keyA)) != out[0].View.OID {
		t.Fatalf("renewal = %+v", out[0])
	}
	must(t, buses[w].DeleteClaim(ctx, keyA, out[0].View.OID))
	if g.remoteRef(ClaimRef(keyA)) != "" {
		t.Fatal("claim not deleted")
	}
}

func TestGitStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	a := g.bus(g.clone("a"), agentA)
	b := g.bus(g.clone("b"), agentB)
	stOID, err := a.WriteState(ctx, sampleState(agentA), "")
	must(t, err)
	_, err = a.WriteOutbox(ctx, sampleOutbox(agentA), "")
	must(t, err)
	if g.remoteRef(StateRef(agentA)) != stOID {
		t.Fatal("state not pushed")
	}

	res, err := b.Fetch(ctx)
	must(t, err)
	st, err := b.Load(ctx)
	must(t, err)
	want := sampleState(agentA)
	want.Format = model.Format
	if !reflect.DeepEqual(st.Agents[agentA], want) || len(st.Outboxes[agentA].Messages) != 1 || len(st.Skipped) != 0 {
		t.Fatalf("loaded %+v, skipped %v", st.Agents[agentA], st.Skipped)
	}
	if st.Main != g.remoteRef("refs/heads/main") || st.Refs[StateRef(agentA)] != stOID {
		t.Fatalf("main/refs = %q/%v", st.Main, st.Refs)
	}

	next := sampleState(agentA)
	next.Heartbeat = t0.Add(time.Minute)
	stOID2, err := a.WriteState(ctx, next, stOID)
	must(t, err)
	res2, err := b.Fetch(ctx)
	must(t, err)
	if res2.Fingerprint == res.Fingerprint {
		t.Fatal("fingerprint did not change")
	}
	st, err = b.Load(ctx)
	must(t, err)
	if st.Refs[StateRef(agentA)] != stOID2 || !st.Agents[agentA].Heartbeat.Equal(next.Heartbeat) {
		t.Fatalf("second load = %+v", st.Agents[agentA])
	}
}

func TestGitAdoptAfterLostPushLog(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repo := g.clone("a")
	oid1, err := g.bus(repo, agentA).WriteState(ctx, sampleState(agentA), "")
	must(t, err)
	// A new process without the push log: its create-only push meets the
	// existing ref ("stale info"), adopts it and retries.
	oid2, err := g.bus(repo, agentA).WriteState(ctx, sampleState(agentA), oid1)
	must(t, err)
	if g.remoteRef(StateRef(agentA)) != oid2 {
		t.Fatal("retry not pushed")
	}
}

func TestGitWipOnDemand(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repoA := g.clone("a")
	a := g.bus(repoA, agentA)
	head, err := repoA.RevParse(ctx, "HEAD")
	must(t, err)
	blob, err := repoA.HashBlob(ctx, []byte("package f\n"))
	must(t, err)
	tree, err := repoA.MakeTree(ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", OID: blob, Name: "f.go"}})
	must(t, err)
	snap, err := repoA.CommitTree(ctx, tree, []string{head}, "gitlet: snapshot", gitx.Identity{})
	must(t, err)
	must(t, a.WriteWip(ctx, itemA, snap))

	repoB := g.clone("b")
	b := g.bus(repoB, agentB)
	got, err := b.FetchWip(ctx, agentA, itemA, snap)
	must(t, err)
	if got != snap {
		t.Fatalf("FetchWip = %q, want %q", got, snap)
	}
	data, err := repoB.ReadFile(ctx, snap, "f.go")
	must(t, err)
	if string(data) != "package f\n" {
		t.Fatalf("snapshot content = %q", data)
	}
	if got, err = b.FetchWip(ctx, agentA, itemA, snap); err != nil || got != snap {
		t.Fatalf("second FetchWip = %q, %v", got, err)
	}
	must(t, b.PruneWips(ctx, nil))
	if _, err := repoB.RevParse(ctx, b.tracking(WipRef(agentA, itemA))); !errors.Is(err, gitx.ErrNotFound) {
		t.Fatalf("wip tracking ref after prune: %v", err)
	}
	must(t, a.DeleteWip(ctx, itemA))
	if g.remoteRef(WipRef(agentA, itemA)) != "" {
		t.Fatal("wip not deleted on the remote")
	}
}

// TestGitWipKeepsUnpushedSecretsLocal: after `git add -A && git commit`
// caught a secret, the snapshot (whose parent is HEAD) is not pushed until
// the commit is on the remote (docs/SPEC.md, section 9.1).
func TestGitWipKeepsUnpushedSecretsLocal(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repo := g.clone("a")
	a := g.bus(repo, agentA)
	git(t, repo.Root, "switch", "-q", "-c", "feature")
	must(t, os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("SECRET=1\n"), 0o644))
	git(t, repo.Root, "add", "-A")
	git(t, repo.Root, "commit", "-q", "-m", "work")
	head, err := repo.RevParse(ctx, "HEAD")
	must(t, err)
	snap, err := repo.Snapshot(ctx, gitx.SnapshotOptions{Parent: head, Message: "gitlet: snapshot"})
	must(t, err)

	var rej *RejectedError
	if err := a.WriteWip(ctx, itemA, snap.Commit); !errors.As(err, &rej) || !strings.Contains(rej.Reason, ".env.local") {
		t.Fatalf("WriteWip: %v, want a RejectedError naming .env.local", err)
	}
	if g.remoteRef(WipRef(agentA, itemA)) != "" {
		t.Fatal("the snapshot reached the remote")
	}
	if exec.Command("git", "-C", g.remote, "cat-file", "-e", head).Run() == nil {
		t.Fatal("the unpushed commit reached the remote")
	}

	git(t, repo.Root, "push", "-q", "origin", "feature")
	must(t, a.WriteWip(ctx, itemA, snap.Commit))
	if g.remoteRef(WipRef(agentA, itemA)) != snap.Commit {
		t.Fatal("snapshot not pushed once its history is on the remote")
	}
}

// TestGitRejectedWipNotFlushed: a snapshot a pre-receive hook declined is
// not pushed again by later flushes.
func TestGitRejectedWipNotFlushed(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repo := g.clone("a")
	a := g.bus(repo, agentA)
	// The hook counts the pushes that reach it and declines snapshots.
	count := filepath.Join(g.dir, "pushes")
	hook := "#!/bin/sh\necho >>'" + count + "'\nwhile read old new ref; do\n" +
		"  case \"$ref\" in refs/gitlet/wip/*) echo 'GH013: secret detected' >&2; exit 1;; esac\ndone\n"
	must(t, os.WriteFile(filepath.Join(g.remote, "hooks", "pre-receive"), []byte(hook), 0o755))
	pushes := func() int {
		data, err := os.ReadFile(count)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		must(t, err)
		return strings.Count(string(data), "\n")
	}

	head, err := repo.RevParse(ctx, "HEAD")
	must(t, err)
	tree, err := repo.RevParse(ctx, "HEAD^{tree}")
	must(t, err)
	snap, err := repo.CommitTree(ctx, tree, []string{head}, "gitlet: snapshot", gitx.Identity{})
	must(t, err)
	var rej *RejectedError
	if err := a.WriteWip(ctx, itemA, snap); !errors.As(err, &rej) || !strings.Contains(strings.Join(rej.Remote, " "), "GH013") {
		t.Fatalf("WriteWip: %v, want a RejectedError with the hook's text", err)
	}
	if _, err := repo.RevParse(ctx, WipRef(agentA, itemA)); !errors.Is(err, gitx.ErrNotFound) {
		t.Fatalf("local wip after the rejection: %v, want none", err)
	}
	before := pushes()
	for range 3 {
		must(t, a.Flush(ctx))
	}
	if n := pushes() - before; n != 0 {
		t.Fatalf("Flush pushed the rejected snapshot %d times", n)
	}
	if g.remoteRef(WipRef(agentA, itemA)) != "" {
		t.Fatal("rejected snapshot on the remote")
	}
}

func TestGitPruneDeletedAgents(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	a := g.bus(g.clone("a"), agentA)
	repoB := g.clone("b")
	b := g.bus(repoB, agentB)
	_, err := a.WriteState(ctx, sampleState(agentA), "")
	must(t, err)
	_, err = a.WriteOutbox(ctx, sampleOutbox(agentA), "")
	must(t, err)
	_, err = b.Fetch(ctx)
	must(t, err)
	st, err := b.Load(ctx)
	must(t, err)
	if _, ok := st.Agents[agentA]; !ok {
		t.Fatal("peer state not loaded")
	}

	must(t, a.DeleteOwn(ctx))
	_, err = b.Fetch(ctx)
	must(t, err)
	st, err = b.Load(ctx)
	must(t, err)
	if _, ok := st.Agents[agentA]; ok || len(st.Refs) != 0 {
		t.Fatalf("deleted agent still loaded: %v", st.Refs)
	}
	refs, err := repoB.ListRefs(ctx, "refs/gitlet-remote/")
	must(t, err)
	if len(refs) != 0 {
		t.Fatalf("tracking refs not pruned: %v", refs)
	}
}

func TestGitRefreshMain(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repo := g.clone("b")
	b := g.bus(repo, agentB)
	next := g.commit("NEWS", "news\n", "second")
	git(t, g.seed, "push", "-q", "origin", "main")
	got, err := b.RefreshMain(ctx)
	must(t, err)
	if got != next {
		t.Fatalf("RefreshMain = %q, want %q", got, next)
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/gitlet-main/origin"} {
		if oid, err := repo.RevParse(ctx, ref); err != nil || oid != next {
			t.Errorf("%s = %q, %v", ref, oid, err)
		}
	}
}

func TestGitPushBranch(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	repo := g.clone("a")
	b := g.bus(repo, agentA)
	dir := repo.Root
	commit := func(content string) string {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", "work.txt")
		git(t, dir, "commit", "-q", "-m", content)
		return git(t, dir, "rev-parse", "HEAD")
	}
	c1 := commit("one")
	r, err := b.PushBranch(ctx, c1, "gitlet/work-1234", "")
	must(t, err)
	if !r.OK || g.remoteRef("refs/heads/gitlet/work-1234") != c1 {
		t.Fatalf("first push = %+v", r)
	}
	git(t, dir, "commit", "-q", "--amend", "-m", "one, amended")
	c2 := git(t, dir, "rev-parse", "HEAD")
	if _, err := b.PushBranch(ctx, c2, "gitlet/work-1234", c1); err != nil {
		t.Fatalf("lease push: %v", err)
	}
	if g.remoteRef("refs/heads/gitlet/work-1234") != c2 {
		t.Fatal("amended commit not pushed")
	}
	git(t, dir, "commit", "-q", "--amend", "-m", "one, amended again")
	c3 := git(t, dir, "rev-parse", "HEAD")
	_, err = b.PushBranch(ctx, c3, "gitlet/work-1234", c1) // c1 is no longer what the remote has
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Kind != gitx.RejectStale {
		t.Fatalf("err = %v, want a stale rejection", err)
	}
}

func TestGitProbe(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	b := g.bus(g.clone("a"), agentA)
	res, err := b.Probe(ctx)
	must(t, err)
	if !res.Create || !res.NonFastForward || !res.Delete {
		t.Fatalf("probe = %+v", res)
	}
	if g.remoteRef(ProbePrefix+string(agentA)) != "" {
		t.Fatal("probe ref left behind")
	}
}

func TestGitSHA256WithoutRemote(t *testing.T) {
	ctx := context.Background()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "repo")
	git(t, filepath.Dir(dir), "init", "-q", "--object-format=sha256", dir)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	repo, err := gitx.Open(ctx, dir)
	must(t, err)
	b := New(repo, agentA, Options{Main: "main"})

	// Create-only local writes: the state ref and a claim.
	stOID, err := b.WriteState(ctx, sampleState(agentA), "")
	must(t, err)
	if len(stOID) != 64 {
		t.Fatalf("state oid %q is not SHA-256", stOID)
	}
	won, view, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), "")
	if err != nil || !won {
		t.Fatalf("CreateClaim = %v, %v", won, err)
	}
	won, lost, err := New(repo, agentB, Options{Main: "main"}).CreateClaim(ctx, sampleClaim(keyA, agentB, itemB), "")
	if err != nil || won || lost.OID != view.OID {
		t.Fatalf("second CreateClaim = %v, %+v, %v; want the first claim", won, lost, err)
	}
}

func TestGitUnreachable(t *testing.T) {
	ctx := context.Background()
	g := newGitEnv(t)
	exe := git(t, g.dir, "--exec-path")
	if _, err := os.Stat(filepath.Join(exe, "git-remote-http")); err != nil {
		t.Skip("git has no http transport")
	}
	for _, k := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	repo := g.clone("a")
	b := g.bus(repo, agentA)
	// Nothing listens on port 1: connection refused, a transport failure.
	git(t, repo.Root, "remote", "set-url", "origin", "http://127.0.0.1:1/remote.git")

	oid1, err := b.WriteState(ctx, sampleState(agentA), "")
	if oid1 == "" || !gitx.IsNetwork(err) {
		t.Fatalf("WriteState = %q, %v; want the local oid and a network error", oid1, err)
	}
	if _, _, err := b.CreateClaim(ctx, sampleClaim(keyA, agentA, itemA), ""); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("CreateClaim: %v, want ErrUnreachable", err)
	}
	if _, err := b.Fetch(ctx); !gitx.IsNetwork(err) {
		t.Fatalf("Fetch: %v, want a network error", err)
	}

	git(t, repo.Root, "remote", "set-url", "origin", g.remote)
	must(t, b.Flush(ctx))
	if g.remoteRef(StateRef(agentA)) != oid1 {
		t.Fatal("Flush did not publish the offline write")
	}
}
