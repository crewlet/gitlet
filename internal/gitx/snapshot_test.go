package gitx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// userState is everything a snapshot must leave alone.
type userState struct {
	index, head, refs, status, staged string
}

func (f *fixture) userState() userState {
	f.t.Helper()
	var s userState
	if _, err := os.Stat(filepath.Join(f.repo.GitDir, "index")); err == nil {
		s.index = fileSHA(f.t, filepath.Join(f.repo.GitDir, "index"))
	}
	s.head = f.git("symbolic-ref", "HEAD") + " " + strings.TrimSpace(gitOut(f.t, f.dir, "rev-parse", "-q", "--verify", "HEAD"))
	s.refs = f.git("for-each-ref")
	s.status = f.git("status", "--porcelain=v2", "-z", "--untracked-files=all")
	s.staged = f.git("ls-files", "--stage")
	return s
}

// gitOut runs git and returns stdout, ignoring failure.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, _ := (&Repo{Root: dir}).exec(bg, cmdOpts{}, args...)
	return string(out)
}

func noTempFiles(t *testing.T, r *Repo) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(r.GitDir, "gitlet", "snap-*")); len(left) != 0 {
		t.Errorf("temporary index left behind: %q", left)
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	head := f.commitFiles("base", map[string]string{
		".gitignore":   "vendor/\n",
		"main.go":      "package main\n",
		"README.md":    "readme\n",
		"config/.env":  "OLD=1\n",
		"data.bin":     "small\n",
		"lib/util.py":  "x = 1\n",
		"unchanged.rs": "fn main() {}\n",
	})
	big := strings.Repeat("x", 2048)
	f.write("main.go", "package main\n\n// changed\n")
	f.write("config/.env", "NEW_SECRET=2\n") // tracked and denied: keeps the old version
	f.write("data.bin", big)                 // tracked and too large: keeps the old version
	os.Remove(filepath.Join(f.dir, "README.md"))
	f.write("new.go", "package main\n")
	f.write("notes.txt", "not source\n")
	f.write("id_rsa", "PRIVATE KEY\n")
	f.write("a.go", big)               // untracked and too large
	f.write("[ab].go", "package ab\n") // a glob, if pathspecs were not literal
	f.write("sp ace.py", "y = 2\n")
	f.write("vendor/dep.go", "package dep\n") // ignored
	f.write("staged.go", "package staged\n")
	f.write("secrets.json", `{"token":"x"}`)
	f.git("add", "staged.go", "secrets.json") // the user staged a secret
	before := f.userState()

	res, err := f.repo.Snapshot(bg, SnapshotOptions{MaxFileSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.git("rev-list", "--parents", "-n1", res.Commit); got != res.Commit+" "+head {
		t.Errorf("snapshot parents = %q, want HEAD", got)
	}
	if tree := f.git("rev-parse", res.Commit+"^{tree}"); tree != res.Tree {
		t.Errorf("Tree = %s, commit's tree = %s", res.Tree, tree)
	}
	if msg := f.git("log", "-1", "--format=%B", res.Commit); msg != "gitlet: snapshot" {
		t.Errorf("message = %q", msg)
	}
	want := map[string]string{
		".gitignore":   "vendor/\n",
		"main.go":      "package main\n\n// changed\n",
		"config/.env":  "OLD=1\n",
		"data.bin":     "small\n",
		"lib/util.py":  "x = 1\n",
		"unchanged.rs": "fn main() {}\n",
		"new.go":       "package main\n",
		"[ab].go":      "package ab\n",
		"sp ace.py":    "y = 2\n",
		"staged.go":    "package staged\n",
	}
	if got := f.treeFiles(res.Tree); !mapsEqual(got, want) {
		t.Errorf("snapshot tree:\n got %q\nwant %q", keys(got), keys(want))
	}
	// id_rsa is not source: the policy, not the deny list, keeps it out.
	if wantSkipped := []string{"a.go", "config/.env", "data.bin", "secrets.json"}; !slices.Equal(res.Skipped, wantSkipped) {
		t.Errorf("Skipped = %q, want %q", res.Skipped, wantSkipped)
	}
	if after := f.userState(); after != before {
		t.Errorf("snapshot changed the user's state:\nbefore %+v\nafter  %+v", before, after)
	}
	noTempFiles(t, f.repo)

	// Nothing changed: the same tree again.
	again, err := f.repo.Background().Snapshot(bg, SnapshotOptions{MaxFileSize: 1024, Message: "gitlet: snapshot 2"})
	if err != nil || again.Tree != res.Tree {
		t.Fatalf("second snapshot = %+v, %v; want tree %s", again, err, res.Tree)
	}
}

func TestSnapshotUntrackedPolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"main.go": "package main\n"})
	f.write("new.go", "package main\n")
	f.write("notes.txt", "notes\n")
	f.write("docs/guide.md", "# guide\n")
	f.write("web/App.TSX", "export {}\n")
	f.write(".env.local", "SECRET=1\n")

	tests := []struct {
		name string
		opt  SnapshotOptions
		want []string // untracked files in the snapshot
	}{
		{"default is source", SnapshotOptions{}, []string{"new.go", "web/App.TSX"}},
		{"source", SnapshotOptions{Untracked: UntrackedSource}, []string{"new.go", "web/App.TSX"}},
		{"none", SnapshotOptions{Untracked: UntrackedNone}, nil},
		{"all", SnapshotOptions{Untracked: UntrackedAll}, []string{"docs/guide.md", "new.go", "notes.txt", "web/App.TSX"}},
		{"include by base name", SnapshotOptions{Include: []string{"*.txt"}}, []string{"new.go", "notes.txt", "web/App.TSX"}},
		{"include by path", SnapshotOptions{Include: []string{"docs/*.md", "[bad"}}, []string{"docs/guide.md", "new.go", "web/App.TSX"}},
		{"custom IsSource", SnapshotOptions{IsSource: func(p string) bool { return strings.HasSuffix(p, ".md") }}, []string{"docs/guide.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := f.repo.Snapshot(bg, tt.opt)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for p := range f.treeFiles(res.Tree) {
				if p != "main.go" {
					got = append(got, p)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("untracked in snapshot = %q, want %q", got, tt.want)
			}
			// Only a policy that would take .env.local reports it skipped.
			var wantSkipped []string
			if tt.opt.Untracked == UntrackedAll {
				wantSkipped = []string{".env.local"}
			}
			if !slices.Equal(res.Skipped, wantSkipped) {
				t.Errorf("Skipped = %q, want %q", res.Skipped, wantSkipped)
			}
		})
	}
}

func TestSnapshotUnbornHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.go", "package a\n")
	f.write("notes.txt", "notes\n")
	f.write("credentials.go", "package secret\n")
	f.write("b.go", "package b\n")
	f.git("add", "a.go", "credentials.go") // staged before the first commit
	before := f.userState()

	res, err := f.repo.Snapshot(bg, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.git("rev-list", "--parents", "-n1", res.Commit); got != res.Commit {
		t.Errorf("snapshot of an unborn HEAD has parents: %q", got)
	}
	if got := f.treeFiles(res.Tree); !mapsEqual(got, map[string]string{"a.go": "package a\n", "b.go": "package b\n"}) {
		t.Errorf("tree = %q", got)
	}
	if !slices.Equal(res.Skipped, []string{"credentials.go"}) {
		t.Errorf("Skipped = %q", res.Skipped)
	}
	if after := f.userState(); after != before {
		t.Errorf("snapshot changed the user's state:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, err := f.repo.RevParse(bg, "HEAD"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HEAD was born: %v", err)
	}
	noTempFiles(t, f.repo)
}

func TestSnapshotWorktreeAndParent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := f.commitFiles("base", map[string]string{"a.go": "package a\n"})
	second := f.commitFiles("second", map[string]string{"a.go": "package a // 2\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	f.git("worktree", "add", "-q", "-b", "wt", wt, base)
	w := openFixture(t, wt)
	w.write("w.go", "package w\n")
	before := w.userState()
	mainIndex := fileSHA(t, filepath.Join(f.repo.GitDir, "index"))

	res, err := w.repo.Snapshot(bg, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.treeFiles(res.Tree); !mapsEqual(got, map[string]string{"a.go": "package a\n", "w.go": "package w\n"}) {
		t.Errorf("worktree snapshot = %q", got)
	}
	if got := w.git("rev-parse", res.Commit+"^"); got != base {
		t.Errorf("parent = %s, want the worktree's HEAD %s", got, base)
	}
	if w.userState() != before || fileSHA(t, filepath.Join(f.repo.GitDir, "index")) != mainIndex {
		t.Error("worktree snapshot changed an index or HEAD")
	}
	noTempFiles(t, w.repo)

	// An explicit parent, and a missing one.
	res, err = f.repo.Snapshot(bg, SnapshotOptions{Parent: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.git("rev-parse", res.Commit+"^"); got != base {
		t.Errorf("explicit parent = %s, want %s", got, base)
	}
	if got := f.treeFiles(res.Tree)["a.go"]; got != "package a // 2\n" {
		t.Errorf("work tree content = %q", got)
	}
	_ = second
	if _, err := f.repo.Snapshot(bg, SnapshotOptions{Parent: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent: %v", err)
	}
}

func TestSnapshotConcurrentAndStaleLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"a.go": "package a\n"})
	f.write("a.go", "package a // changed\n")
	f.write("b.go", "package b\n")
	// A lock left by a killed git must not block the next snapshot.
	dir := filepath.Join(f.repo.GitDir, "gitlet")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("snap-%d.index.lock", os.Getpid())), nil, 0o644)

	var wg sync.WaitGroup
	trees := make([]string, 6)
	errs := make([]error, 6)
	for i := range trees {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.repo.Background().Snapshot(bg, SnapshotOptions{})
			trees[i], errs[i] = res.Tree, err
		}()
	}
	wg.Wait()
	for i := range trees {
		if errs[i] != nil || trees[i] != trees[0] {
			t.Errorf("snapshot %d = %s, %v", i, trees[i], errs[i])
		}
	}
	noTempFiles(t, f.repo)
}

func TestSnapshotDuringConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"a.go": "package a\n", "secrets.yaml": "k: 0\n"})
	f.git("switch", "-q", "-c", "side")
	f.commitFiles("side", map[string]string{"a.go": "package a // side\n", "secrets.yaml": "k: side\n"})
	f.git("switch", "-q", "main")
	f.commitFiles("main", map[string]string{"a.go": "package a // main\n", "secrets.yaml": "k: main\n"})
	gitFails(t, f.dir, "merge", "side")
	before := f.userState()

	res, err := f.repo.Snapshot(bg, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	files := f.treeFiles(res.Tree)
	if !strings.Contains(files["a.go"], "<<<<<<<") || files["secrets.yaml"] != "k: main\n" {
		t.Errorf("snapshot during a conflict = %q", files)
	}
	if f.userState() != before {
		t.Error("snapshot changed the user's state")
	}
}

func TestIsDenied(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		".env":                 true,
		"app/.env.local":       true,
		".envrc":               true,
		"certs/server.pem":     true,
		"tls.key":              true,
		"store.p12":            true,
		"a/b/c.pfx":            true,
		"id_rsa":               true,
		"home/.ssh/id_rsa.pub": true,
		"id_ed25519":           true,
		"id_ecdsa":             true,
		"release.keystore":     true,
		"credentials":          true,
		"aws/credentials.json": true,
		"secrets.yaml":         true,
		"SECRETS.YAML":         true,
		"conf\\id_rsa":         true,
		"main.go":              false,
		"environment.go":       false,
		"keyboard.go":          false,
		"my.env":               false,
		"secret.go":            false,
		"key.go":               false,
		"pem/readme.md":        false,
		".env/":                false,
		"":                     false,
	}
	for p, want := range tests {
		if got := IsDenied(p); got != want {
			t.Errorf("IsDenied(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestPushExcluded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	big := strings.Repeat("x", 2048)
	f.commitFiles("base", map[string]string{
		"main.go":      "package main\n",
		".env.example": "KEY=\n",
		"fixture.bin":  big,
		"same.txt":     "SECRET=shared\n",
	})
	f.git("remote", "add", "origin", newBare(t))
	f.git("push", "-q", "origin", "main")
	published := []string{"refs/remotes/origin/"}
	check := func(what string, published []string, maxSize int64, want ...string) {
		t.Helper()
		got, err := f.repo.PushExcluded(bg, f.git("rev-parse", "HEAD"), published, maxSize)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: PushExcluded = %q, %v; want %q", what, got, err, want)
		}
	}
	check("published history", published, 1024)

	f.commitFiles("local", map[string]string{
		"config/.env.local":     "SECRET=1\n",
		"data/big.bin":          big + "y",
		"credentials/readme.md": "a directory named like a denied file\n",
		"sp ace.key":            "k\n",
		".env":                  "SECRET=shared\n", // the same blob as same.txt, already published
	})
	f.git("rm", "-q", "config/.env.local")
	f.commitFiles("remove the secret", map[string]string{"main.go": "package main // 2\n"})
	check("unpushed commits", published, 1024, "config/.env.local", "data/big.bin", "sp ace.key")
	check("no size limit", published, 0, "config/.env.local", "sp ace.key")
	// Now the walk meets the shared blob as .env first.
	check("nothing published", nil, 1024, ".env", ".env.example", "config/.env.local", "data/big.bin", "fixture.bin", "sp ace.key")

	f.git("push", "-q", "origin", "main")
	check("after the push", published, 1024)

	head := f.git("rev-parse", "HEAD")
	for _, bad := range []struct {
		commit    string
		published []string
	}{{"--all", nil}, {head, []string{"--all"}}, {head, []string{"refs/x\nrefs/y"}}} {
		if _, err := f.repo.PushExcluded(bg, bad.commit, bad.published, 0); err == nil {
			t.Errorf("PushExcluded(%q, %q) succeeded", bad.commit, bad.published)
		}
	}
	if _, err := f.repo.PushExcluded(bg, strings.Repeat("1", len(head)), nil, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("PushExcluded of a missing commit: %v, want ErrNotFound", err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
