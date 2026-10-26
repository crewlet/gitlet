package sim

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/gitlet/bench/internal/gitenv"
)

// ConflictStats measures a conflicting merge the way an agent meets it: the
// files, the conflict hunks and the lines inside the markers.
type ConflictStats struct {
	Files []string `json:"files"`
	Hunks int      `json:"hunks"`
	Lines int      `json:"lines"`
}

// mergeTree merges two commits without a work tree. It returns the result
// tree, and the conflict stats when the merge conflicts.
func mergeTree(ctx context.Context, env *gitenv.Env, dir, ours, theirs string) (tree string, conflict *ConflictStats, err error) {
	r, err := env.Run(ctx, dir, nil, "git", "merge-tree", "--write-tree", "--name-only", "--no-messages", ours, theirs)
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimRight(r.Stdout, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", nil, fmt.Errorf("merge-tree %s %s: exit %d: %s", ours, theirs, r.Code, r.Stderr)
	}
	tree = lines[0]
	switch r.Code {
	case 0:
		return tree, nil, nil
	case 1:
	default:
		return "", nil, fmt.Errorf("merge-tree %s %s: exit %d: %s", ours, theirs, r.Code, r.Stderr)
	}
	cs := &ConflictStats{}
	seen := map[string]bool{}
	for _, f := range lines[1:] {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		cs.Files = append(cs.Files, f)
		blob, err := env.Run(ctx, dir, nil, "git", "cat-file", "-p", tree+":"+f)
		if err != nil {
			return "", nil, err
		}
		h, l := countMarkers(blob.Stdout)
		cs.Hunks += h
		cs.Lines += l
	}
	if cs.Hunks == 0 {
		cs.Hunks = len(cs.Files) // modify/delete or rename conflicts have no markers
	}
	return tree, cs, nil
}

// countMarkers counts conflict hunks and the lines between their markers.
func countMarkers(s string) (hunks, lines int) {
	in := false
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "<<<<<<< "):
			in = true
			hunks++
		case strings.HasPrefix(l, ">>>>>>> "):
			in = false
		case strings.HasPrefix(l, "=======") && in:
		case in:
			lines++
		}
	}
	return hunks, lines
}

// diffLines counts changed lines (added plus deleted) between two trees or
// commits.
func diffLines(ctx context.Context, env *gitenv.Env, dir, a, b string) (int, error) {
	out, err := env.Git(ctx, dir, "diff", "--numstat", "--no-renames", a, b)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		add, _ := strconv.Atoi(f[0])
		del, _ := strconv.Atoi(f[1])
		n += add + del
	}
	return n, nil
}

// isAncestor reports whether a is an ancestor of b.
func isAncestor(ctx context.Context, env *gitenv.Env, dir, a, b string) bool {
	r, err := env.Run(ctx, dir, nil, "git", "merge-base", "--is-ancestor", a, b)
	return err == nil && r.Code == 0
}
