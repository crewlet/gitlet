package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (r *Repo) diffNameStatus(ctx context.Context, from, to string) ([]DiffEntry, error) {
	if from == "" {
		empty, err := r.emptyTree(ctx)
		if err != nil {
			return nil, err
		}
		from = empty
	}
	if err := errors.Join(checkRev(from), checkRev(to)); err != nil {
		return nil, err
	}
	// diff-tree is plumbing, so diff.* settings of the user do not change it.
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "diff-tree", "-r", "-z", "-M", "--name-status", "--no-commit-id", from, to, "--")
	if err != nil {
		return nil, mapNotFound(err, from+" or "+to)
	}
	return parseNameStatus(splitNUL(out))
}

// parseNameStatus reads diff --name-status -z output: a status field, then
// one path, or two (old, new) for renames and copies.
func parseNameStatus(fields []string) ([]DiffEntry, error) {
	var list []DiffEntry
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			return nil, fmt.Errorf("gitx: malformed diff output")
		}
		e := DiffEntry{Status: status[:1]}
		switch e.Status {
		case "R", "C":
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("gitx: malformed diff output")
			}
			e.OldPath, e.Path = fields[i+1], fields[i+2]
			i += 2
		default:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("gitx: malformed diff output")
			}
			e.Path = fields[i+1]
			i++
		}
		list = append(list, e)
	}
	return list, nil
}

func (r *Repo) diffNames(ctx context.Context, from, to string) ([]string, error) {
	entries, err := r.diffNameStatus(ctx, from, to)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Path)
		if e.OldPath != "" {
			names = append(names, e.OldPath)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// emptyTree returns the id of the empty tree in the repository's hash.
func (r *Repo) emptyTree(ctx context.Context) (string, error) {
	return r.read(ctx, "hash-object", "-t", "tree", "--stdin")
}

func (r *Repo) status(ctx context.Context) ([]StatusEntry, error) {
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parseStatus(splitNUL(out))
}

// parseStatus reads git status --porcelain=v2 -z records. Header ("#") and
// ignored ("!") records are skipped.
func parseStatus(fields []string) ([]StatusEntry, error) {
	var list []StatusEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch f[0] {
		case '1': // 1 XY sub mH mI mW hH hI path
			p := strings.SplitN(f, " ", 9)
			if len(p) != 9 {
				return nil, fmt.Errorf("gitx: malformed status record %q", f)
			}
			list = append(list, StatusEntry{XY: p[1], Path: p[8]})
		case '2': // 2 XY sub mH mI mW hH hI Xscore path, then the original path
			p := strings.SplitN(f, " ", 10)
			if len(p) != 10 || i+1 >= len(fields) {
				return nil, fmt.Errorf("gitx: malformed status record %q", f)
			}
			list = append(list, StatusEntry{XY: p[1], Path: p[9], OrigPath: fields[i+1]})
			i++
		case 'u': // u XY sub m1 m2 m3 mW h1 h2 h3 path
			p := strings.SplitN(f, " ", 11)
			if len(p) != 11 {
				return nil, fmt.Errorf("gitx: malformed status record %q", f)
			}
			list = append(list, StatusEntry{XY: p[1], Path: p[10]})
		case '?':
			list = append(list, StatusEntry{XY: "??", Path: strings.TrimPrefix(f, "? "), Untracked: true})
		case '#', '!':
		default:
			return nil, fmt.Errorf("gitx: malformed status record %q", f)
		}
	}
	return list, nil
}

func (r *Repo) isClean(ctx context.Context) (bool, error) {
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "status", "--porcelain", "-z", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return len(out) == 0, nil
}

func (r *Repo) worktreeFingerprint(ctx context.Context) (string, error) {
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead},
		"status", "--porcelain=v2", "--branch", "--no-ahead-behind", "-z", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	fields := splitNUL(out)
	h := sha256.New()
	for _, f := range fields {
		// HEAD and the branch count; the upstream and its distance do not
		// change the work tree.
		if strings.HasPrefix(f, "# ") && !strings.HasPrefix(f, "# branch.oid ") && !strings.HasPrefix(f, "# branch.head ") {
			continue
		}
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	entries, err := parseStatus(fields)
	if err != nil {
		return "", err
	}
	// A file edited again after its first change keeps its status line;
	// its size or modification time tells the edits apart.
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(r.Root, filepath.FromSlash(e.Path)))
		if err != nil {
			fmt.Fprintf(h, "%s\x00missing\x00", e.Path)
			continue
		}
		fmt.Fprintf(h, "%s\x00%d %d %o\x00", e.Path, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
