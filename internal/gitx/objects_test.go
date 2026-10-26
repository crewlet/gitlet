package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMakeTreeAndCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	blob, err := f.repo.HashBlob(bg, []byte(`{"format":1}`))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := f.repo.HashBlob(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := f.repo.MakeTree(bg, []TreeEntry{{Mode: "100644", Type: "blob", OID: empty, Name: "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := f.repo.MakeTree(bg, []TreeEntry{
		{Mode: "100644", Type: "blob", OID: blob, Name: "state.json"},
		{Mode: "040000", Type: "tree", OID: sub, Name: "dir"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.treeFiles(tree); len(got) != 2 || got["state.json"] != `{"format":1}` || got["dir/empty"] != "" {
		t.Fatalf("tree = %q", got)
	}

	when := time.Date(2026, 10, 26, 2, 0, 0, 0, time.FixedZone("", 2*3600))
	id := Identity{Name: "Agent Smith", Email: "smith@example.com", When: when}
	root, err := f.repo.CommitTree(bg, tree, nil, "gitlet: state", id)
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.repo.CommitTree(bg, tree, []string{root}, "gitlet: claim\n\nbody line", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.repo.ShowCommit(bg, root)
	if err != nil {
		t.Fatal(err)
	}
	if info.OID != root || info.Author != "Agent Smith" || info.AuthorEmail != "smith@example.com" ||
		!info.When.Equal(when) || info.Subject != "gitlet: state" || info.Message != "gitlet: state" {
		t.Fatalf("ShowCommit = %+v", info)
	}
	if _, off := info.When.Zone(); off != 2*3600 {
		t.Errorf("time zone offset = %d", off)
	}
	if c := f.git("cat-file", "commit", root); !strings.Contains(c, "committer Agent Smith <smith@example.com> 1792972800 +0200") {
		t.Errorf("committer not set from Identity:\n%s", c)
	}
	if got := f.git("rev-list", "--parents", "-n1", child); got != child+" "+root {
		t.Errorf("parents = %q", got)
	}
	info, _ = f.repo.ShowCommit(bg, child)
	if info.Author != "Gitx Test" || info.Message != "gitlet: claim\n\nbody line" || info.Subject != "gitlet: claim" {
		t.Errorf("zero Identity commit = %+v", info)
	}

	bad := []struct {
		name    string
		entries []TreeEntry
	}{
		{"slash", []TreeEntry{{"100644", "blob", blob, "a/b"}}},
		{"empty name", []TreeEntry{{"100644", "blob", blob, ""}}},
		{"dot dot", []TreeEntry{{"100644", "blob", blob, ".."}}},
		{"nul", []TreeEntry{{"100644", "blob", blob, "a\x00b"}}},
		{"space in oid", []TreeEntry{{"100644", "blob", blob + " x", "a"}}},
		{"duplicate", []TreeEntry{{"100644", "blob", blob, "a"}, {"100644", "blob", blob, "a"}}},
		{"missing object", []TreeEntry{{"100644", "blob", strings.Repeat("1", 40), "a"}}},
	}
	for _, tt := range bad {
		if _, err := f.repo.MakeTree(bg, tt.entries); err == nil {
			t.Errorf("MakeTree(%s) succeeded", tt.name)
		}
	}
	if _, err := f.repo.CommitTree(bg, tree, nil, "x", Identity{Name: "a <b>"}); err == nil {
		t.Error("CommitTree accepted a name with angle brackets")
	}
}

func TestCommitNeverSigned(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := f.commit("one")
	// Signing would fail: gitlet's commits must not try.
	f.git("config", "commit.gpgSign", "true")
	f.git("config", "gpg.program", "false")
	tree := f.git("rev-parse", "HEAD^{tree}")
	c, err := f.repo.CommitTree(bg, tree, []string{base}, "gitlet: state", Identity{})
	if err != nil {
		t.Fatalf("CommitTree with commit.gpgSign=true: %v", err)
	}
	if strings.Contains(f.git("cat-file", "commit", c), "gpgsig") {
		t.Error("commit is signed")
	}
	if _, err := f.repo.CommitFile(bg, base, "stub.go", []byte("package stub\n"), "gitlet stub: X", Identity{}); err != nil {
		t.Errorf("CommitFile with commit.gpgSign=true: %v", err)
	}
	f.write("new.go", "package main\n")
	if _, err := f.repo.Snapshot(bg, SnapshotOptions{}); err != nil {
		t.Errorf("Snapshot with commit.gpgSign=true: %v", err)
	}
}

func TestFallbackIdentity(t *testing.T) {
	f := newFixture(t)
	tree := f.git("hash-object", "-t", "tree", "--stdin")
	// No identity anywhere, and no guessing from the host.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	r, err := Open(bg, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(bg, "var", "GIT_AUTHOR_IDENT"); err == nil {
		t.Skip("git still finds an identity")
	}

	c, err := r.CommitTree(bg, tree, nil, "gitlet: state", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.ShowCommit(bg, c)
	if err != nil || info.Author != FallbackIdentity.Name || info.AuthorEmail != FallbackIdentity.Email {
		t.Fatalf("fallback commit = %+v, %v", info, err)
	}
	// A partial identity keeps its own part.
	c, err = r.CommitTree(bg, tree, nil, "gitlet: state", Identity{Name: "Only Name"})
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := r.ShowCommit(bg, c); info.Author != "Only Name" || info.AuthorEmail != FallbackIdentity.Email {
		t.Fatalf("partial identity commit = %+v", info)
	}
	// Git's own identity still wins over the fallback.
	t.Setenv("GIT_AUTHOR_NAME", "Env Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "env@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Env Author")
	t.Setenv("GIT_COMMITTER_EMAIL", "env@example.com")
	r, _ = Open(bg, f.dir)
	c, err = r.CommitTree(bg, tree, nil, "gitlet: state", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := r.ShowCommit(bg, c); info.Author != "Env Author" {
		t.Fatalf("env identity commit = %+v", info)
	}
}

func TestCommitFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"src/config.ts": "old\n", "run.sh": "#!/bin/sh\n", "docs": "a file\n"})
	if err := os.Chmod(filepath.Join(f.dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := f.commit("exec bit")
	if mode := strings.Fields(f.git("ls-tree", base, "run.sh"))[0]; mode != "100755" {
		t.Fatalf("fixture: run.sh mode = %s", mode)
	}
	f.write("src/config.ts", "dirty work\n")
	indexBefore := fileSHA(t, filepath.Join(f.repo.GitDir, "index"))
	statusBefore := f.git("status", "--porcelain")

	c, err := f.repo.CommitFile(bg, "main", "src/config.ts", []byte("stub\n"), "gitlet stub: parseConfig", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	files := f.treeFiles(c)
	if files["src/config.ts"] != "stub\n" || files["run.sh"] != "#!/bin/sh\n" || len(files) != 3 {
		t.Fatalf("CommitFile tree = %q", files)
	}
	if got := f.git("rev-parse", c+"^"); got != base {
		t.Errorf("parent = %s, want %s", got, base)
	}

	c2, err := f.repo.CommitFile(bg, base, "run.sh", []byte("#!/bin/sh\necho hi\n"), "x", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if mode := strings.Fields(f.git("ls-tree", c2, "run.sh"))[0]; mode != "100755" {
		t.Errorf("run.sh mode = %s, want 100755 kept", mode)
	}
	// A new path in a new directory, and a path through what was a file.
	c3, err := f.repo.CommitFile(bg, base, "pkg/new/x.go", []byte("package x\n"), "x", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.treeFiles(c3)["pkg/new/x.go"]; got != "package x\n" {
		t.Errorf("new file = %q", got)
	}
	c4, err := f.repo.CommitFile(bg, base, "docs/index.md", []byte("# docs\n"), "x", Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if files := f.treeFiles(c4); files["docs/index.md"] != "# docs\n" {
		t.Errorf("file replaced by directory: %q", files)
	}

	if f.read("src/config.ts") != "dirty work\n" || f.git("status", "--porcelain") != statusBefore ||
		fileSHA(t, filepath.Join(f.repo.GitDir, "index")) != indexBefore || f.git("rev-parse", "HEAD") != base {
		t.Error("CommitFile touched the work tree, index or HEAD")
	}
	if left, _ := filepath.Glob(filepath.Join(f.repo.GitDir, "gitlet", "*")); len(left) != 0 {
		t.Errorf("temporary files left: %q", left)
	}

	for _, p := range []string{"", "/abs", "a/../b", "./a", "a//b", ".git/config", "x/.GIT/y", "dir/"} {
		if _, err := f.repo.CommitFile(bg, base, p, nil, "x", Identity{}); err == nil {
			t.Errorf("CommitFile(%q) succeeded", p)
		}
	}
	if _, err := f.repo.CommitFile(bg, "nope", "a", nil, "x", Identity{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("CommitFile(missing base) = %v", err)
	}
}

func TestUpdateRef(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c1 := f.commit("one")
	c2 := f.commit("two")
	const ref = "refs/gitlet/claims/abc"

	if err := f.repo.UpdateRef(bg, ref, c1, ZeroOID); err != nil {
		t.Fatalf("create: %v", err)
	}
	var e *Error
	if err := f.repo.UpdateRef(bg, ref, c2, ZeroOID); !errors.As(err, &e) || !strings.Contains(e.Stderr, "exists") {
		t.Fatalf("second create = %v, want *Error about the existing ref", err)
	}
	if err := f.repo.UpdateRef(bg, ref, c2, c2); !errors.As(err, &e) || !strings.Contains(e.Stderr, "expected") {
		t.Fatalf("stale update = %v, want *Error about the mismatch", err)
	}
	if err := f.repo.UpdateRef(bg, ref, c2, c1); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := f.repo.UpdateRef(bg, ref, c1, ""); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}
	if err := f.repo.DeleteRef(bg, ref, c2); err == nil {
		t.Fatal("stale delete succeeded")
	}
	if err := f.repo.DeleteRef(bg, ref, c1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.repo.DeleteRef(bg, ref, ""); err != nil {
		t.Fatalf("delete of a missing ref: %v", err)
	}
	if _, err := f.repo.RevParse(bg, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ref still there: %v", err)
	}
	if err := f.repo.UpdateRef(bg, "-d", c1, ""); err == nil {
		t.Fatal("UpdateRef accepted an option as ref")
	}
}

func TestListRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit("base")
	tree := f.git("rev-parse", "HEAD^{tree}")
	at := func(day int) string {
		c, err := f.repo.CommitTree(bg, tree, nil, "x", Identity{When: time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	old, mid, newest := at(1), at(10), at(20)
	refs := map[string]string{
		"refs/gitlet/agents/a/state":    mid,
		"refs/gitlet/claims/k1":         old,
		"refs/gitlet/claims/k2":         newest,
		"refs/gitlet-remote/o/claims/k": old,
		"refs/heads/feature":            mid,
	}
	for ref, oid := range refs {
		f.git("update-ref", ref, oid)
	}
	blob := f.git("hash-object", "-w", "--stdin")
	f.git("update-ref", "refs/gitlet/claims/blob", blob)

	got, err := f.repo.ListRefs(bg, "refs/gitlet/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["refs/gitlet/claims/k2"] != newest || got["refs/gitlet/claims/blob"] != blob {
		t.Fatalf("ListRefs(refs/gitlet/) = %v", got)
	}
	if got, _ := f.repo.ListRefs(bg, "refs/gitlet/claims/k"); len(got) != 2 {
		t.Fatalf("ListRefs(partial name) = %v", got)
	}
	if got, err := f.repo.ListRefs(bg, "refs/nothing/"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ListRefs(missing namespace) = %v, %v", got, err)
	}

	byDate, err := f.repo.ListRefsByDate(bg, "refs/gitlet/", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ri := range byDate {
		names = append(names, ri.Name)
	}
	want := []string{"refs/gitlet/claims/k2", "refs/gitlet/agents/a/state", "refs/gitlet/claims/k1"}
	if !slices.Equal(names, want) {
		t.Fatalf("ListRefsByDate = %q, want %q (blobs skipped)", names, want)
	}
	if !byDate[0].CommitterDate.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) || byDate[0].OID != newest {
		t.Errorf("newest = %+v", byDate[0])
	}
	if top, _ := f.repo.ListRefsByDate(bg, "refs/gitlet/", 1); len(top) != 1 || top[0].Name != want[0] {
		t.Errorf("ListRefsByDate(max 1) = %+v", top)
	}
}
