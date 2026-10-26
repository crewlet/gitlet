package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// divergent builds main and feature branches from a common base. Each map
// holds the files its branch's single commit writes. It leaves main checked out.
func divergent(t *testing.T, base, onMain, onFeature map[string]string) *fixture {
	t.Helper()
	f := newFixture(t)
	f.commitFiles("base", base)
	f.git("switch", "-q", "-c", "feature")
	f.commitFiles("feature work", onFeature)
	f.git("switch", "-q", "main")
	f.commitFiles("main work", onMain)
	return f
}

func TestMergeTree(t *testing.T) {
	t.Parallel()
	base := map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "b\n", "c.txt": "c\n"}
	clean := divergent(t, base, map[string]string{"a.txt": "one\n2\n3\n"}, map[string]string{"b.txt": "bee\n"})
	res, err := clean.repo.MergeTree(bg, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clean || len(res.Conflicts) != 0 {
		t.Fatalf("clean merge = %+v", res)
	}
	if files := clean.treeFiles(res.Tree); files["a.txt"] != "one\n2\n3\n" || files["b.txt"] != "bee\n" {
		t.Errorf("merged tree = %q", files)
	}

	conflict := divergent(t, base,
		map[string]string{"a.txt": "main\n2\n3\n", "c.txt": "main c\n"},
		map[string]string{"a.txt": "feature\n2\n3\n", "c.txt": "feature c\n", "b.txt": "bee\n"})
	before := conflict.userState()
	res, err = conflict.repo.MergeTree(bg, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean || !slices.Equal(res.Conflicts, []string{"a.txt", "c.txt"}) || res.Tree == "" {
		t.Fatalf("conflicting merge = %+v", res)
	}
	if conflict.userState() != before {
		t.Error("merge-tree touched the work tree")
	}
	if _, err := conflict.repo.MergeTree(bg, "main", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MergeTree(missing) = %v", err)
	}
}

func TestRebaseConflictAborts(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "main\n2\n3\n"},
		map[string]string{"a.txt": "feature\n2\n3\n"})
	f.git("switch", "-q", "feature")
	f.commitFiles("feature more", map[string]string{"c.txt": "c\n"})
	head := f.git("rev-parse", "HEAD")
	f.write("b.txt", "dirty\n") // autostashed and given back

	res, err := f.repo.Rebase(bg, RebaseOptions{Upstream: "main", Autostash: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !slices.Equal(res.Conflicts, []string{"a.txt"}) {
		t.Fatalf("Rebase = %+v, want a conflict on a.txt", res)
	}
	if op, _ := f.repo.Operation(bg); op != "" {
		t.Fatalf("rebase left in progress: %q", op)
	}
	if b, _ := f.repo.CurrentBranch(bg); b != "feature" || f.git("rev-parse", "HEAD") != head {
		t.Fatalf("branch not restored: %s at %s", b, f.git("rev-parse", "HEAD"))
	}
	if f.read("b.txt") != "dirty\n" || f.read("a.txt") != "feature\n2\n3\n" {
		t.Fatal("work tree not restored")
	}
	if stash := f.git("stash", "list"); stash != "" {
		t.Fatalf("autostash left behind: %s", stash)
	}

	// Without autostash a dirty tree is refused before anything starts.
	if _, err := f.repo.Rebase(bg, RebaseOptions{Upstream: "main"}); err == nil {
		t.Fatal("Rebase of a dirty tree without autostash succeeded")
	}
	if op, _ := f.repo.Operation(bg); op != "" {
		t.Fatalf("refused rebase left %q", op)
	}
}

func TestRebaseClean(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "a\n"},
		map[string]string{"m.txt": "m\n"},
		map[string]string{"f.txt": "f\n"})
	f.git("switch", "-q", "feature")
	f.commitFiles("drop me", map[string]string{"stub.txt": "stub\n"})
	f.commitFiles("keep me", map[string]string{"k.txt": "k\n"})

	// The sequence editor rewrites the todo, as gitlet's hook does. Newer git
	// versions write "pick <oid> # <subject>".
	script := filepath.Join(t.TempDir(), "editor.sh")
	err := os.WriteFile(script, []byte("#!/bin/sh\nsed -e 's/^pick \\([0-9a-f]*\\) \\(# \\)\\{0,1\\}drop me$/drop \\1 drop me/' \"$1\" > \"$1.tmp\" && mv \"$1.tmp\" \"$1\"\n"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	f.git("config", "rebase.abbreviateCommands", "true") // must not reach the editor
	res, err := f.repo.Rebase(bg, RebaseOptions{Upstream: "main", SequenceEditor: "sh " + script})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Head != f.git("rev-parse", "HEAD") {
		t.Fatalf("Rebase = %+v", res)
	}
	if ok, _ := f.repo.IsAncestor(bg, "main", "HEAD"); !ok {
		t.Error("main is not an ancestor after the rebase")
	}
	if subjects := f.git("log", "--format=%s", "main..HEAD"); subjects != "keep me\nfeature work" {
		t.Errorf("rebased commits = %q", subjects)
	}
}

func TestRebaseDetachedFromCancel(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "a\n"}, map[string]string{"m.txt": "m\n"}, map[string]string{"f.txt": "f\n"})
	f.git("switch", "-q", "feature")
	ctx, cancel := context.WithCancel(bg)
	time.AfterFunc(200*time.Millisecond, cancel)
	// The editor outlives the caller's context; the rebase still finishes.
	res, err := f.repo.Rebase(ctx, RebaseOptions{Upstream: "main", SequenceEditor: "sleep 1; true"})
	if err != nil || !res.OK {
		t.Fatalf("Rebase after cancel = %+v, %v", res, err)
	}
	if ctx.Err() == nil {
		t.Fatal("context was not cancelled during the rebase")
	}
	if _, err := f.repo.Rebase(ctx, RebaseOptions{Upstream: "main"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Rebase with a cancelled context = %v, want it refused", err)
	}
}

func TestRebaseBusy(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "a\n"}, map[string]string{"a.txt": "m\n"}, map[string]string{"a.txt": "f\n"})
	gitFails(t, f.dir, "merge", "feature")
	before := f.userState()
	if _, err := f.repo.Rebase(bg, RebaseOptions{Upstream: "feature"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Rebase during a merge = %v, want ErrBusy", err)
	}
	if _, err := f.repo.CherryPick(bg, "feature"); !errors.Is(err, ErrBusy) {
		t.Fatalf("CherryPick during a merge = %v, want ErrBusy", err)
	}
	if f.userState() != before {
		t.Fatal("refused operation changed the repository")
	}
}

func TestCherryPickAndRevert(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "1\n2\n3\n", "s.txt": "s\n"},
		map[string]string{"a.txt": "main\n2\n3\n"},
		map[string]string{"stub.go": "package stub\n"})
	stub := f.git("rev-parse", "feature")

	res, err := f.repo.CherryPick(bg, stub)
	if err != nil || !res.OK {
		t.Fatalf("CherryPick = %+v, %v", res, err)
	}
	if msg := f.git("log", "-1", "--format=%B"); !strings.Contains(msg, "(cherry picked from commit "+stub+")") {
		t.Errorf("message lacks -x line: %q", msg)
	}
	picked := res.Head

	res, err = f.repo.Revert(bg, picked)
	if err != nil || !res.OK {
		t.Fatalf("Revert = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "stub.go")); !errors.Is(err, os.ErrNotExist) {
		t.Error("revert left stub.go")
	}

	// Conflicts abort and restore HEAD.
	base := f.git("rev-list", "--max-parents=0", "HEAD")
	f.git("switch", "-q", "-c", "other", base)
	conflicting := f.commitFiles("other", map[string]string{"a.txt": "other\n2\n3\n"})
	f.git("switch", "-q", "main")
	head := f.git("rev-parse", "HEAD")
	res, err = f.repo.CherryPick(bg, conflicting)
	if err != nil || res.OK || !slices.Equal(res.Conflicts, []string{"a.txt"}) {
		t.Fatalf("conflicting CherryPick = %+v, %v", res, err)
	}
	if op, _ := f.repo.Operation(bg); op != "" || f.git("rev-parse", "HEAD") != head || f.read("a.txt") != "main\n2\n3\n" {
		t.Fatalf("cherry-pick not aborted (op %q)", op)
	}
	res, err = f.repo.Revert(bg, base) // base created a.txt, which main changed since
	if err != nil || res.OK || !slices.Equal(res.Conflicts, []string{"a.txt"}) {
		t.Fatalf("conflicting Revert = %+v, %v", res, err)
	}
	if op, _ := f.repo.Operation(bg); op != "" || f.git("rev-parse", "HEAD") != head {
		t.Fatalf("revert not aborted (op %q)", op)
	}

	// A pick that changes nothing stops without conflicts: aborted, and an error.
	if _, err := f.repo.CherryPick(bg, head); err == nil {
		t.Fatal("empty cherry-pick succeeded")
	}
	if op, _ := f.repo.Operation(bg); op != "" || f.git("rev-parse", "HEAD") != head {
		t.Fatalf("empty cherry-pick not aborted (op %q)", op)
	}
	if _, err := f.repo.CherryPick(bg, "nope"); err == nil {
		t.Fatal("CherryPick(missing) succeeded")
	}
}

func TestApplyMarksItsCommitsInternal(t *testing.T) {
	t.Parallel()
	f := divergent(t, map[string]string{"a.txt": "1\n"},
		map[string]string{"m.txt": "m\n"},
		map[string]string{"stub.go": "package stub\n"})
	hooks := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	script := "#!/bin/sh\necho \"$1 ${" + InternalEnv + ":-unset}\" >> '" + seen + "'\n"
	for _, name := range []string{"prepare-commit-msg", "post-commit"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.git("config", "core.hooksPath", hooks)

	res, err := f.repo.CherryPick(bg, f.git("rev-parse", "feature"))
	if err != nil || !res.OK {
		t.Fatalf("CherryPick = %+v, %v", res, err)
	}
	if res, err = f.repo.Revert(bg, res.Head); err != nil || !res.OK {
		t.Fatalf("Revert = %+v, %v", res, err)
	}
	f.git("switch", "-q", "feature")
	if res, err = f.repo.Rebase(bg, RebaseOptions{Upstream: "main"}); err != nil || !res.OK {
		t.Fatalf("Rebase = %+v, %v", res, err)
	}
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 4 {
		t.Fatalf("hooks ran %d times: %q", len(lines), data)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, " 1") {
			t.Errorf("a hook ran without %s=1: %q", InternalEnv, l)
		}
	}
}

func TestSwitchAndCreateBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	f.write("b.txt", "dirty\n")

	if err := f.repo.CreateBranch(bg, "gitlet/strict-mode-8c1f", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.repo.CurrentBranch(bg); b != "gitlet/strict-mode-8c1f" || f.read("b.txt") != "dirty\n" {
		t.Fatalf("CreateBranch: on %q, b.txt %q", b, f.read("b.txt"))
	}
	if up := gitOut(t, f.dir, "config", "branch.gitlet/strict-mode-8c1f.merge"); up != "" {
		t.Errorf("new branch tracks %q", up)
	}
	if err := f.repo.CreateBranch(bg, "gitlet/strict-mode-8c1f", ""); err == nil {
		t.Error("CreateBranch of an existing branch succeeded")
	}
	f.git("switch", "-q", "main")
	if err := f.repo.CreateBranch(bg, "from-start", "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Switch(bg, "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.repo.CurrentBranch(bg); b != "main" || f.read("b.txt") != "dirty\n" {
		t.Fatalf("Switch: on %q, b.txt %q", b, f.read("b.txt"))
	}

	// A switch that would overwrite local changes is refused.
	f.git("switch", "-q", "-c", "topic")
	f.git("checkout", "-q", "--", "b.txt")
	f.commitFiles("topic", map[string]string{"a.txt": "topic\n"})
	f.git("switch", "-q", "main")
	f.write("a.txt", "local edit\n")
	if err := f.repo.Switch(bg, "topic"); err == nil {
		t.Fatal("Switch over local changes succeeded")
	}
	if b, _ := f.repo.CurrentBranch(bg); b != "main" || f.read("a.txt") != "local edit\n" {
		t.Fatal("refused switch changed the work tree")
	}
	if err := f.repo.Switch(bg, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Switch(missing) = %v, want ErrNotFound", err)
	}
	for _, name := range []string{"", "-c", "a b"} {
		if err := f.repo.CreateBranch(bg, name, ""); err == nil {
			t.Errorf("CreateBranch(%q) succeeded", name)
		}
	}
}
