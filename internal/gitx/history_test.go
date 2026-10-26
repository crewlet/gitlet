package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// commitAt commits everything with author and committer dates set to when.
func (f *fixture) commitAt(msg string, when time.Time) string {
	f.t.Helper()
	date := when.Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	gitIn(f.t, f.dir, env, "add", "-A")
	gitIn(f.t, f.dir, env, "commit", "-q", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

func TestLogAndShow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	c1 := f.commitAt("first", day(1))
	f.write("a.txt", "a\n")
	c2 := f.commitAt("second\n\nGitlet-Item: 8c1f0e2a9b3d\nGitlet-Agent: claude-code-laptop-3fa9c1", day(10))
	c3 := f.commitAt("third line one\nline two\n\nbody", day(20))

	all, err := f.repo.Log(bg, "", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var oids []string
	for _, c := range all {
		oids = append(oids, c.OID)
	}
	if !slices.Equal(oids, []string{c3, c2, c1}) {
		t.Fatalf("Log = %q", oids)
	}
	if all[0].Subject != "third line one line two" || all[0].Message != "third line one\nline two\n\nbody" {
		t.Errorf("subject/message = %q / %q", all[0].Subject, all[0].Message)
	}
	if !all[1].When.Equal(day(10)) || all[1].Author != "Gitx Test" || all[1].AuthorEmail != "gitx@example.com" {
		t.Errorf("Log[1] = %+v", all[1])
	}
	if recent, _ := f.repo.Log(bg, "HEAD", day(5), 0); len(recent) != 2 {
		t.Errorf("Log(since) = %d commits, want 2", len(recent))
	}
	if top, _ := f.repo.Log(bg, "main", time.Time{}, 1); len(top) != 1 || top[0].OID != c3 {
		t.Errorf("Log(max 1) = %+v", top)
	}
	if _, err := f.repo.Log(bg, "nope", time.Time{}, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Log(missing) = %v", err)
	}

	info, err := f.repo.ShowCommit(bg, "HEAD~1")
	if err != nil {
		t.Fatal(err)
	}
	if info.OID != c2 || info.Subject != "second" || info.Message != "second\n\nGitlet-Item: 8c1f0e2a9b3d\nGitlet-Agent: claude-code-laptop-3fa9c1" {
		t.Errorf("ShowCommit = %+v", info)
	}
	if _, err := f.repo.ShowCommit(bg, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ShowCommit(missing) = %v", err)
	}

	if got, err := f.repo.LogGrep(bg, c1+"..main", "Gitlet-Item: 8c1f0e2a9b3d"); err != nil || !slices.Equal(got, []string{c2}) {
		t.Errorf("LogGrep = %q, %v", got, err)
	}
	if got, err := f.repo.LogGrep(bg, c2+"..main", "Gitlet-Item: 8c1f0e2a9b3d"); err != nil || len(got) != 0 {
		t.Errorf("LogGrep(outside range) = %q, %v", got, err)
	}
	if got, _ := f.repo.LogGrep(bg, "main", "Gitlet-Item: 8c1f.*"); len(got) != 0 {
		t.Errorf("LogGrep treated text as a pattern: %q", got)
	}
	if _, err := f.repo.LogGrep(bg, "main", ""); err == nil {
		t.Error("LogGrep with empty text succeeded")
	}

	if got, err := f.repo.RevList(bg, "main", c1); err != nil || !slices.Equal(got, []string{c3, c2}) {
		t.Errorf("RevList = %q, %v", got, err)
	}
	if got, err := f.repo.RevList(bg, "main", ""); err != nil || len(got) != 3 {
		t.Errorf("RevList(no exclude) = %q, %v", got, err)
	}
	if _, err := f.repo.RevList(bg, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevList(missing) = %v", err)
	}
}

func TestLogUnborn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.repo.Log(bg, "", time.Time{}, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Log(unborn) = %v, want ErrNotFound", err)
	}
}

func TestGrepAndBlame(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c1 := f.commitFiles("one", map[string]string{
		"src/a.ts":    "const c = parseConfig(x)\n",
		"src/b.ts":    "parseConfigStrict(y)\n",
		"src/c.ts":    "// parseConfig\n",
		"bin.dat":     "parseConfig\x00\x01\x02",
		"lines.txt":   "one\ntwo\nthree\n",
		"dir/my f.go": "parseConfig()\n",
	})
	c2 := f.commitFiles("two", map[string]string{"lines.txt": "one\nTWO\nthree\n"})

	got, err := f.repo.Grep(bg, "HEAD", "parseConfig", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"dir/my f.go", "src/a.ts", "src/c.ts"}) {
		t.Errorf("Grep = %q (whole words, binary skipped)", got)
	}
	if got, _ := f.repo.Grep(bg, c1, "parseConfig", 2); len(got) != 2 {
		t.Errorf("Grep(max 2) = %q", got)
	}
	if got, err := f.repo.Grep(bg, "HEAD", "nowhere", 0); err != nil || got != nil {
		t.Errorf("Grep(no match) = %q, %v", got, err)
	}
	if got, _ := f.repo.Grep(bg, "HEAD", "parse.*", 0); len(got) != 0 {
		t.Errorf("Grep treated the word as a pattern: %q", got)
	}
	if _, err := f.repo.Grep(bg, "nope", "x", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Grep(missing rev) = %v", err)
	}
	for _, w := range []string{"", "a\nb"} {
		if _, err := f.repo.Grep(bg, "HEAD", w, 0); err == nil {
			t.Errorf("Grep(%q) succeeded", w)
		}
	}

	for line, want := range map[int]string{1: c1, 2: c2, 3: c1} {
		if got, err := f.repo.BlameLine(bg, "lines.txt", line); err != nil || got != want {
			t.Errorf("BlameLine(%d) = %s, %v; want %s", line, got, err, want)
		}
	}
	if got, err := f.repo.BlameLine(bg, "dir/my f.go", 1); err != nil || got != c1 {
		t.Errorf("BlameLine(path with space) = %s, %v", got, err)
	}
	for _, c := range []struct {
		path string
		line int
	}{{"lines.txt", 99}, {"missing.txt", 1}} {
		if _, err := f.repo.BlameLine(bg, c.path, c.line); !errors.Is(err, ErrNotFound) {
			t.Errorf("BlameLine(%s, %d) = %v, want ErrNotFound", c.path, c.line, err)
		}
	}
	if _, err := f.repo.BlameLine(bg, "lines.txt", 0); err == nil {
		t.Error("BlameLine(line 0) succeeded")
	}
}

func TestStatusAndDiff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c1 := f.commitFiles("one", map[string]string{
		"keep.txt":     "keep\n",
		"move/old.txt": "the same content, long enough to be detected as a rename\n",
		"gone.txt":     "gone\n",
		".gitignore":   "*.log\n",
	})
	if clean, err := f.repo.IsClean(bg); err != nil || !clean {
		t.Fatalf("IsClean = %v, %v", clean, err)
	}

	f.write("keep.txt", "changed\n")
	f.git("mv", "move/old.txt", "move/new.txt")
	f.write("dir/ünï côde.txt", "x\n")
	f.write("added.txt", "added\n")
	f.git("add", "added.txt")
	f.write("debug.log", "ignored\n")
	os.Remove(filepath.Join(f.dir, "gone.txt"))

	got, err := f.repo.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	want := []StatusEntry{
		{XY: "A.", Path: "added.txt"},
		{XY: ".D", Path: "gone.txt"},
		{XY: ".M", Path: "keep.txt"},
		{XY: "R.", Path: "move/new.txt", OrigPath: "move/old.txt"},
		{XY: "??", Path: "dir/ünï côde.txt", Untracked: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Status =\n%+v\nwant\n%+v", got, want)
	}
	if clean, err := f.repo.IsClean(bg); err != nil || clean {
		t.Fatalf("IsClean(dirty) = %v, %v", clean, err)
	}

	c2 := f.commit("two")
	entries, err := f.repo.DiffNameStatus(bg, c1, c2)
	if err != nil {
		t.Fatal(err)
	}
	wantDiff := []DiffEntry{
		{Status: "A", Path: "added.txt"},
		{Status: "A", Path: "dir/ünï côde.txt"},
		{Status: "D", Path: "gone.txt"},
		{Status: "M", Path: "keep.txt"},
		{Status: "R", Path: "move/new.txt", OldPath: "move/old.txt"},
	}
	if !slices.Equal(entries, wantDiff) {
		t.Fatalf("DiffNameStatus =\n%+v\nwant\n%+v", entries, wantDiff)
	}
	names, err := f.repo.DiffNames(bg, c1, c2)
	if err != nil || !slices.Equal(names, []string{"added.txt", "dir/ünï côde.txt", "gone.txt", "keep.txt", "move/new.txt", "move/old.txt"}) {
		t.Fatalf("DiffNames = %q, %v", names, err)
	}
	if all, err := f.repo.DiffNameStatus(bg, "", c1); err != nil || len(all) != 4 || all[0].Status != "A" {
		t.Fatalf("DiffNameStatus(empty tree) = %+v, %v", all, err)
	}
	if _, err := f.repo.DiffNameStatus(bg, c1, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DiffNameStatus(missing) = %v", err)
	}
}

func TestWorktreeFingerprint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("one", map[string]string{"a.txt": "aaaa\n"})
	fp := func() string {
		t.Helper()
		s, err := f.repo.Background().WorktreeFingerprint(bg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	clean := fp()
	if fp() != clean {
		t.Fatal("fingerprint not stable")
	}
	f.write("a.txt", "bbbb\n")
	edited := fp()
	if edited == clean {
		t.Fatal("edit not seen")
	}
	// Same size, same status line: the modification time tells them apart.
	f.write("a.txt", "cccc\n")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(f.dir, "a.txt"), later, later)
	if fp() == edited {
		t.Fatal("second edit of a modified file not seen")
	}
	again := fp()
	f.write("new.go", "package x\n")
	if fp() == again {
		t.Fatal("untracked file not seen")
	}
	os.Remove(filepath.Join(f.dir, "new.go"))
	f.git("checkout", "--", "a.txt")
	if fp() != clean {
		t.Fatal("fingerprint differs after returning to a clean tree")
	}
	f.commit("empty")
	if fp() == clean {
		t.Fatal("new HEAD not seen")
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	fields := []string{
		"# branch.oid 1234",
		"1 .M N... 100644 100644 100644 aaaa bbbb with space.txt",
		"2 R. N... 100644 100644 100644 aaaa aaaa R100 new name.txt", "old name.txt",
		"u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflict.txt",
		"? untracked dir/file",
		"! ignored.log",
	}
	got, err := parseStatus(fields)
	if err != nil {
		t.Fatal(err)
	}
	want := []StatusEntry{
		{XY: ".M", Path: "with space.txt"},
		{XY: "R.", Path: "new name.txt", OrigPath: "old name.txt"},
		{XY: "UU", Path: "conflict.txt"},
		{XY: "??", Path: "untracked dir/file", Untracked: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseStatus =\n%+v\nwant\n%+v", got, want)
	}
	for _, bad := range [][]string{{"1 .M short"}, {"2 R. N... 1 2 3 a b R100 only-new"}, {"x what"}} {
		if _, err := parseStatus(bad); err == nil {
			t.Errorf("parseStatus(%q) succeeded", bad)
		}
	}
}
