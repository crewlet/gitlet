package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// remoteFixture is a bare remote with two clones, a and b, acting as two
// machines.
type remoteFixture struct {
	bare string
	a, b *fixture
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	bare := newBare(t)
	a := newFixture(t)
	a.commitFiles("base", map[string]string{"a.txt": "a\n"})
	a.git("remote", "add", "origin", bare)
	a.git("push", "-q", "origin", "main")
	dir := filepath.Join(t.TempDir(), "b")
	gitIn(t, filepath.Dir(dir), nil, "clone", "-q", bare, dir)
	return &remoteFixture{bare: bare, a: a, b: openFixture(t, dir)}
}

// metaCommit makes a parentless metadata commit like the bus writes.
func (f *fixture) metaCommit(content string) string {
	f.t.Helper()
	blob, err := f.repo.HashBlob(bg, []byte(content))
	if err != nil {
		f.t.Fatal(err)
	}
	tree, err := f.repo.MakeTree(bg, []TreeEntry{{Mode: "100644", Type: "blob", OID: blob, Name: "claim.json"}})
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := f.repo.CommitTree(bg, tree, nil, "gitlet: claim", Identity{})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// pushOne pushes a single metadata update and returns its result.
func (f *fixture) pushOne(u PushUpdate) (PushResult, PushReport) {
	f.t.Helper()
	rep, err := f.repo.Push(bg, PushOptions{Remote: "origin", Metadata: true, Updates: []PushUpdate{u}})
	if err != nil {
		f.t.Fatalf("Push(%+v): %v", u, err)
	}
	if len(rep.Results) != 1 {
		f.t.Fatalf("Push(%+v): %d results", u, len(rep.Results))
	}
	return rep.Results[0], rep
}

func TestPushLeases(t *testing.T) {
	t.Parallel()
	rf := newRemoteFixture(t)
	a, b := rf.a, rf.b
	const claim = "refs/gitlet/claims/0123456789abcdef"
	ca1, ca2, cb := a.metaCommit("a1"), a.metaCommit("a2"), b.metaCommit("b")
	remoteRef := func() string {
		t.Helper()
		refs, err := a.repo.LsRemote(bg, "origin", claim)
		if err != nil {
			t.Fatal(err)
		}
		return refs[claim]
	}

	// Create-only race: both saw the claim absent, the remote picks one.
	if res, _ := a.pushOne(PushUpdate{Src: ca1, Dst: claim, Expect: ExpectAbsent}); !res.OK || res.Dst != claim {
		t.Fatalf("first create = %+v", res)
	}
	res, _ := b.pushOne(PushUpdate{Src: cb, Dst: claim, Expect: ExpectAbsent})
	if res.OK || res.Kind != RejectStale || res.Reason != "stale info" {
		t.Fatalf("second create = %+v, want a stale rejection", res)
	}
	if got := remoteRef(); got != ca1 {
		t.Fatalf("remote claim = %s, want the winner %s", got, ca1)
	}

	// Learning the holder: a metadata fetch of the claims namespace.
	os.Remove(filepath.Join(b.repo.GitDir, "FETCH_HEAD"))
	err := b.repo.Fetch(bg, FetchOptions{Remote: "origin", Prune: true, Metadata: true,
		Refspecs: []string{"+refs/gitlet/claims/*:refs/gitlet-remote/origin/claims/*"}})
	if err != nil {
		t.Fatal(err)
	}
	tracking := "refs/gitlet-remote/origin/claims/0123456789abcdef"
	if refs, _ := b.repo.ListRefs(bg, "refs/gitlet-remote/origin/"); refs[tracking] != ca1 {
		t.Fatalf("tracking refs after fetch = %v", refs)
	}
	if _, err := os.Stat(filepath.Join(b.repo.GitDir, "FETCH_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("metadata fetch wrote FETCH_HEAD: %v", err)
	}

	// Update with a lease: a stale lease is refused, the right one wins even
	// though the new commit does not descend from the old one.
	if res, _ := b.pushOne(PushUpdate{Src: cb, Dst: claim, Expect: ExpectOID, OldOID: cb}); res.Kind != RejectStale {
		t.Fatalf("stale update = %+v", res)
	}
	if res, _ := a.pushOne(PushUpdate{Src: ca2, Dst: claim, Expect: ExpectOID, OldOID: ca1}); !res.OK || res.UpToDate {
		t.Fatalf("leased update = %+v", res)
	}
	if res, _ := a.pushOne(PushUpdate{Src: ca2, Dst: claim, Expect: ExpectOID, OldOID: ca2}); !res.OK || !res.UpToDate {
		t.Fatalf("repeated update = %+v, want up to date", res)
	}

	// Delete with a lease.
	if res, _ := b.pushOne(PushUpdate{Dst: claim, Expect: ExpectOID, OldOID: ca1}); res.OK || res.Kind != RejectStale {
		t.Fatalf("stale delete = %+v", res)
	}
	if res, _ := b.pushOne(PushUpdate{Dst: claim, Expect: ExpectOID, OldOID: ca2}); !res.OK {
		t.Fatalf("leased delete = %+v", res)
	}
	if got := remoteRef(); got != "" {
		t.Fatalf("claim still on the remote: %s", got)
	}
	err = b.repo.Fetch(bg, FetchOptions{Remote: "origin", Prune: true, Metadata: true,
		Refspecs: []string{"+refs/gitlet/claims/*:refs/gitlet-remote/origin/claims/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if refs, _ := b.repo.ListRefs(bg, "refs/gitlet-remote/"); len(refs) != 0 {
		t.Fatalf("prune left %v", refs)
	}

	// ZeroOID as the expected value means "must not exist", like ExpectAbsent.
	if res, _ := b.pushOne(PushUpdate{Src: cb, Dst: claim, Expect: ExpectOID, OldOID: ZeroOID}); !res.OK {
		t.Fatalf("create with ZeroOID lease = %+v", res)
	}
}

func TestPushBatchAndRejections(t *testing.T) {
	t.Parallel()
	rf := newRemoteFixture(t)
	a, b := rf.a, rf.b
	c1, c2 := a.metaCommit("1"), a.metaCommit("2")
	if res, _ := a.pushOne(PushUpdate{Src: c1, Dst: "refs/gitlet/claims/k1", Expect: ExpectAbsent}); !res.OK {
		t.Fatalf("setup push = %+v", res)
	}

	// One push, a lease per ref, not atomic: each ref has its own result.
	rep, err := a.repo.Push(bg, PushOptions{Remote: "origin", Metadata: true, Updates: []PushUpdate{
		{Src: c2, Dst: "refs/gitlet/claims/k1", Expect: ExpectOID, OldOID: c2},
		{Src: c2, Dst: "refs/gitlet/claims/k2", Expect: ExpectAbsent},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PushResult{}
	for _, r := range rep.Results {
		got[r.Dst] = r
	}
	if r := got["refs/gitlet/claims/k1"]; r.OK || r.Kind != RejectStale {
		t.Errorf("k1 = %+v, want stale", r)
	}
	if r := got["refs/gitlet/claims/k2"]; !r.OK {
		t.Errorf("k2 = %+v, want OK", r)
	}

	// A branch push that does not fast-forward.
	b.commitFiles("b side", map[string]string{"b.txt": "b\n"})
	b.git("push", "-q", "origin", "main")
	a.commitFiles("a side", map[string]string{"c.txt": "c\n"})
	res, _ := a.pushOne(PushUpdate{Src: "HEAD", Dst: "refs/heads/main"})
	if res.OK || res.Kind != RejectNonFF {
		t.Errorf("non-fast-forward push = %+v", res)
	}
	if res, _ := a.pushOne(PushUpdate{Src: "HEAD", Dst: "refs/heads/main", Expect: Force}); !res.OK {
		t.Errorf("forced push = %+v", res)
	}

	// The remote refuses wip refs: a remote rejection with its message.
	hook := "#!/bin/sh\nwhile read old new ref; do\n  case \"$ref\" in refs/gitlet/wip/*) echo \"wip refs are not allowed here\" >&2; exit 1;; esac\ndone\n"
	if err := os.WriteFile(filepath.Join(rf.bare, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	res, rep = a.pushOne(PushUpdate{Src: "HEAD", Dst: "refs/gitlet/wip/agent/item", Expect: ExpectAbsent})
	if res.OK || res.Kind != RejectRemote || res.Reason != "pre-receive hook declined" {
		t.Errorf("wip push = %+v", res)
	}
	if !slices.Contains(rep.Remote, "wip refs are not allowed here") {
		t.Errorf("remote lines = %q", rep.Remote)
	}

	// Failures before any ref is tried are errors.
	_, err = a.repo.Push(bg, PushOptions{Remote: "origin", Updates: []PushUpdate{{Src: "nope", Dst: "refs/heads/x"}}})
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Stderr, "does not match any") {
		t.Errorf("push of a missing source = %v", err)
	}
	_, err = a.repo.Push(bg, PushOptions{Remote: filepath.Join(t.TempDir(), "gone.git"), Metadata: true,
		Updates: []PushUpdate{{Src: c1, Dst: "refs/gitlet/claims/k3", Expect: ExpectAbsent}}})
	if !IsNetwork(err) {
		t.Errorf("push to a missing remote = %v, want a network error", err)
	}
	for _, u := range []PushUpdate{
		{Src: c1, Dst: ""},
		{Src: c1, Dst: "refs/a:refs/b"},
		{Src: "+" + c1, Dst: "refs/a"},
		{Src: c1, Dst: "refs/a", Expect: Expect(9)},
	} {
		if _, err := a.repo.Push(bg, PushOptions{Remote: "origin", Updates: []PushUpdate{u}}); err == nil {
			t.Errorf("Push(%+v) succeeded", u)
		}
	}
	if rep, err := a.repo.Push(bg, PushOptions{Remote: "origin"}); err != nil || len(rep.Results) != 0 {
		t.Errorf("empty Push = %+v, %v", rep, err)
	}
}

func TestLsRemoteAndFetch(t *testing.T) {
	t.Parallel()
	rf := newRemoteFixture(t)
	a, b := rf.a, rf.b
	c := a.metaCommit("x")
	for _, ref := range []string{"refs/gitlet/agents/a1/state", "refs/gitlet/claims/k"} {
		if res, _ := a.pushOne(PushUpdate{Src: c, Dst: ref, Expect: ExpectAbsent}); !res.OK {
			t.Fatalf("push %s = %+v", ref, res)
		}
	}
	a.git("tag", "-a", "-m", "v1", "v1")
	a.git("push", "-q", "origin", "v1")

	refs, err := b.repo.LsRemote(bg, "origin", "refs/gitlet/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs["refs/gitlet/claims/k"] != c {
		t.Fatalf("LsRemote(refs/gitlet/*) = %v", refs)
	}
	if refs, _ := b.repo.LsRemote(bg, "origin", "v1"); len(refs) != 1 {
		t.Fatalf("LsRemote(v1) = %v, want the tag without its peeled line", refs)
	}

	// Main goes to gitlet's own ref; the user's remote-tracking ref and tags
	// are left alone.
	before := b.git("rev-parse", "refs/remotes/origin/main")
	a.commitFiles("more", map[string]string{"m.txt": "m\n"})
	a.git("push", "-q", "origin", "main")
	err = b.repo.Fetch(bg, FetchOptions{Remote: "origin", Prune: true, Metadata: true, Refspecs: []string{
		"+refs/gitlet/agents/*:refs/gitlet-remote/origin/agents/*",
		"+refs/gitlet/claims/*:refs/gitlet-remote/origin/claims/*",
		"+refs/heads/main:refs/gitlet-main/origin",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := b.repo.RevParse(bg, "refs/gitlet-main/origin"); got != a.git("rev-parse", "HEAD") {
		t.Errorf("gitlet main = %s", got)
	}
	if got := b.git("rev-parse", "refs/remotes/origin/main"); got != before {
		t.Error("metadata fetch moved refs/remotes/origin/main")
	}
	if _, err := b.repo.RevParse(bg, "refs/tags/v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("metadata fetch brought tags: %v", err)
	}
	if refs, _ := b.repo.ListRefs(bg, "refs/gitlet-remote/origin/"); len(refs) != 2 {
		t.Errorf("tracking refs = %v", refs)
	}

	// A wip that is gone: ErrNotFound, with git's error kept, and not the network.
	err = b.repo.Fetch(bg, FetchOptions{Remote: "origin", Metadata: true,
		Refspecs: []string{"+refs/gitlet/wip/a1/gone:refs/gitlet-remote/origin/wip/a1/gone"}})
	var e *Error
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &e) || !strings.Contains(e.Stderr, "couldn't find remote ref") || IsNetwork(err) {
		t.Errorf("fetch of a missing ref = %v, want ErrNotFound wrapping *Error", err)
	}
	if err := b.repo.Fetch(bg, FetchOptions{Remote: "--upload-pack=evil"}); err == nil {
		t.Error("Fetch accepted an option as remote")
	}
	if _, err := b.repo.LsRemote(bg, filepath.Join(t.TempDir(), "gone.git")); !IsNetwork(err) {
		t.Errorf("LsRemote(missing remote) = %v, want a network error", err)
	}
}

func TestFetchEmptyRemote(t *testing.T) {
	t.Parallel()
	bare := newBare(t)
	f := newFixture(t)
	f.commit("local only")
	f.git("remote", "add", "origin", bare)
	poll := []string{
		"+refs/gitlet/agents/*:refs/gitlet-remote/origin/agents/*",
		"+refs/gitlet/claims/*:refs/gitlet-remote/origin/claims/*",
	}
	// No main on the remote yet: the poll fails cleanly as ErrNotFound...
	err := f.repo.Fetch(bg, FetchOptions{Remote: "origin", Prune: true, Metadata: true,
		Refspecs: append(slices.Clone(poll), "+refs/heads/main:refs/gitlet-main/origin")})
	if !errors.Is(err, ErrNotFound) || IsNetwork(err) || IsPermission(err) {
		t.Fatalf("fetch of a missing main = %v, want ErrNotFound", err)
	}
	if refs, _ := f.repo.ListRefs(bg, "refs/gitlet-main/"); len(refs) != 0 {
		t.Fatalf("failed fetch wrote %v", refs)
	}
	// ...and globs that match nothing are fine.
	if err := f.repo.Fetch(bg, FetchOptions{Remote: "origin", Prune: true, Metadata: true, Refspecs: poll}); err != nil {
		t.Fatalf("fetch without main = %v", err)
	}
}

func TestParsePorcelain(t *testing.T) {
	t.Parallel()
	out := strings.Join([]string{
		"To git@example.com:o/r.git",
		"*\trefs/heads/x:refs/gitlet/claims/new\t[new reference]",
		"+\tabc:refs/gitlet/claims/forced\t1111111...2222222 (forced update)",
		" \trefs/heads/main:refs/heads/main\t1111111..2222222",
		"-\t:refs/gitlet/wip/a/i\t[deleted]",
		"=\trefs/heads/main:refs/heads/same\t[up to date]",
		"!\tabc:refs/gitlet/claims/stale\t[rejected] (stale info)",
		"!\tabc:refs/heads/behind\t[rejected] (fetch first)",
		"!\tabc:refs/heads/nonff\t[rejected] (non-fast-forward)",
		"!\tabc:refs/gitlet/wip/a/j\t[remote rejected] (pre-receive hook declined)",
		"!\tabc:refs/gitlet/claims/raced\t[remote rejected] (failed to update ref)",
		"!\tabc:refs/gitlet/claims/gh\t[remote rejected] (incorrect old value provided)",
		"!\tabc:refs/gitlet/claims/lock\t[remote rejected] (cannot lock ref 'refs/gitlet/claims/lock': is at 1 but expected 2)",
		"!\tabc:refs/heads/protected\t[remote rejected] (protected branch hook declined)",
		"!\tabc:refs/heads/quiet\t[remote failure] (remote failed to report status)",
		"Done",
	}, "\n")
	remote := []string{"error: cannot lock ref 'refs/gitlet/claims/raced': reference already exists"}
	want := []PushResult{
		{Dst: "refs/gitlet/claims/new", OK: true},
		{Dst: "refs/gitlet/claims/forced", OK: true},
		{Dst: "refs/heads/main", OK: true},
		{Dst: "refs/gitlet/wip/a/i", OK: true},
		{Dst: "refs/heads/same", OK: true, UpToDate: true},
		{Dst: "refs/gitlet/claims/stale", Kind: RejectStale, Reason: "stale info"},
		{Dst: "refs/heads/behind", Kind: RejectNonFF, Reason: "fetch first"},
		{Dst: "refs/heads/nonff", Kind: RejectNonFF, Reason: "non-fast-forward"},
		{Dst: "refs/gitlet/wip/a/j", Kind: RejectRemote, Reason: "pre-receive hook declined"},
		{Dst: "refs/gitlet/claims/raced", Kind: RejectStale, Reason: "failed to update ref"},
		{Dst: "refs/gitlet/claims/gh", Kind: RejectStale, Reason: "incorrect old value provided"},
		{Dst: "refs/gitlet/claims/lock", Kind: RejectStale, Reason: "cannot lock ref 'refs/gitlet/claims/lock': is at 1 but expected 2"},
		{Dst: "refs/heads/protected", Kind: RejectRemote, Reason: "protected branch hook declined"},
		{Dst: "refs/heads/quiet", Kind: RejectOther, Reason: "remote failed to report status"},
	}
	if got := parsePorcelain(out, remote); !slices.Equal(got, want) {
		t.Fatalf("parsePorcelain =\n%+v\nwant\n%+v", got, want)
	}
	// Without the remote's message, a failed ref update is the remote's refusal.
	if got := parsePorcelain("!\tabc:refs/x\t[remote rejected] (failed to update ref)", nil); got[0].Kind != RejectRemote {
		t.Errorf("failed to update ref without context = %+v", got[0])
	}

	stderr := "remote: Resolving deltas: 100% (1/1)\rremote: Resolving deltas: 100% (1/1), done.        \n" +
		"remote: \nremote: Create a pull request:\x1b[K\nerror: failed to push some refs\n"
	if got := remoteLines(stderr); !slices.Equal(got, []string{"Resolving deltas: 100% (1/1)", "Resolving deltas: 100% (1/1), done.", "Create a pull request:"}) {
		t.Errorf("remoteLines = %q", got)
	}
}

func TestPushSpec(t *testing.T) {
	t.Parallel()
	const old = "1111111111111111111111111111111111111111"
	tests := []struct {
		u           PushUpdate
		lease, spec string
	}{
		{PushUpdate{Src: "c", Dst: "refs/heads/b"}, "", "c:refs/heads/b"},
		{PushUpdate{Src: "c", Dst: "refs/x", Expect: ExpectAbsent}, "--force-with-lease=refs/x:", "c:refs/x"},
		{PushUpdate{Src: "c", Dst: "refs/x", Expect: ExpectOID, OldOID: old}, "--force-with-lease=refs/x:" + old, "c:refs/x"},
		{PushUpdate{Src: "c", Dst: "refs/x", Expect: ExpectOID, OldOID: ZeroOID}, "--force-with-lease=refs/x:", "c:refs/x"},
		{PushUpdate{Src: "c", Dst: "refs/x", Expect: ExpectOID}, "--force-with-lease=refs/x:", "c:refs/x"},
		{PushUpdate{Src: "c", Dst: "refs/x", Expect: Force}, "", "+c:refs/x"},
		{PushUpdate{Dst: "refs/x", Expect: ExpectOID, OldOID: old}, "--force-with-lease=refs/x:" + old, ":refs/x"},
		{PushUpdate{Dst: "refs/x", Expect: Force}, "", ":refs/x"},
	}
	for _, tt := range tests {
		lease, spec, err := pushSpec(tt.u)
		if err != nil || lease != tt.lease || spec != tt.spec {
			t.Errorf("pushSpec(%+v) = %q, %q, %v; want %q, %q", tt.u, lease, spec, err, tt.lease, tt.spec)
		}
	}
}
