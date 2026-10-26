package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// logFormat prints one commit: id, author, email, author date, then the raw
// message, which goes last because only it may hold arbitrary text.
const logFormat = "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%B"

func (r *Repo) blameLine(ctx context.Context, path string, line int) (string, error) {
	if err := checkTreePath(path); err != nil {
		return "", err
	}
	if line < 1 {
		return "", fmt.Errorf("gitx: blame: invalid line %d", line)
	}
	l := strconv.Itoa(line)
	out, err := r.read(ctx, "blame", "--porcelain", "-L", l+","+l, "HEAD", "--", path)
	if err != nil {
		// "file x has only 3 lines": the line does not exist.
		if strings.Contains(stderrOf(err), " has only ") {
			return "", notFound(path + ":" + l)
		}
		return "", mapNotFound(err, path+":"+l)
	}
	// The first header line is "<oid> <orig line> <final line> <count>".
	oid, _, _ := strings.Cut(out, " ")
	if oid == "" || strings.Contains(oid, "\n") {
		return "", fmt.Errorf("gitx: blame: unexpected output for %s:%d", path, line)
	}
	return oid, nil
}

func (r *Repo) showCommit(ctx context.Context, rev string) (CommitInfo, error) {
	if err := checkRev(rev); err != nil {
		return CommitInfo{}, err
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "log", "-1", "-z", "--no-show-signature", logFormat, rev, "--")
	if err != nil {
		return CommitInfo{}, mapNotFound(err, rev)
	}
	commits, err := parseLog(out)
	if err != nil {
		return CommitInfo{}, err
	}
	if len(commits) == 0 {
		return CommitInfo{}, notFound(rev)
	}
	return commits[0], nil
}

func (r *Repo) log(ctx context.Context, rev string, since time.Time, max int) ([]CommitInfo, error) {
	if rev == "" {
		rev = "HEAD"
	}
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 100
	}
	args := []string{"log", "-z", "--no-show-signature", logFormat, "-n", strconv.Itoa(max)}
	if !since.IsZero() {
		args = append(args, "--max-age="+strconv.FormatInt(since.Unix(), 10))
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, append(args, rev, "--")...)
	if err != nil {
		return nil, mapNotFound(err, rev)
	}
	return parseLog(out)
}

// parseLog reads NUL-separated records printed with logFormat.
func parseLog(out []byte) ([]CommitInfo, error) {
	var list []CommitInfo
	for _, rec := range splitNUL(out) {
		rec = strings.TrimPrefix(rec, "\n") // separator newline before records after the first
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 5)
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: malformed log record %q", rec)
		}
		when, err := time.Parse(time.RFC3339, f[3])
		if err != nil {
			return nil, fmt.Errorf("gitx: log: bad date %q", f[3])
		}
		msg := strings.TrimRight(f[4], "\n")
		list = append(list, CommitInfo{
			OID: f[0], Author: f[1], AuthorEmail: f[2], When: when,
			Subject: subject(msg), Message: msg,
		})
	}
	return list, nil
}

// subject is git's %s: the first paragraph of the message on one line.
func subject(msg string) string {
	var parts []string
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, " ")
}

func (r *Repo) logGrep(ctx context.Context, revRange, text string) ([]string, error) {
	if err := checkRev(revRange); err != nil {
		return nil, err
	}
	// An empty pattern would match every commit.
	if text == "" || strings.ContainsAny(text, "\x00\n") {
		return nil, fmt.Errorf("gitx: log: invalid search text %q", text)
	}
	out, err := r.read(ctx, "log", "--no-show-signature", "--format=%H", "-F", "--grep="+text, revRange, "--")
	if err != nil {
		return nil, mapNotFound(err, revRange)
	}
	return lines(out), nil
}

func (r *Repo) revList(ctx context.Context, include, exclude string) ([]string, error) {
	if err := checkRev(include); err != nil {
		return nil, err
	}
	args := []string{"rev-list", include}
	if exclude != "" {
		if err := checkRev(exclude); err != nil {
			return nil, err
		}
		args = append(args, "^"+exclude)
	}
	out, err := r.read(ctx, append(args, "--")...)
	if err != nil {
		return nil, mapNotFound(err, include)
	}
	return lines(out), nil
}

func (r *Repo) grep(ctx context.Context, rev, word string, max int) ([]string, error) {
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	// A newline would make git search for several patterns.
	if word == "" || strings.ContainsAny(word, "\x00\n") {
		return nil, fmt.Errorf("gitx: grep: invalid word %q", word)
	}
	if max <= 0 {
		max = 50
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead},
		"grep", "-w", "-F", "-z", "-l", "-I", "--no-color", "-e", word, rev, "--")
	if err != nil {
		if exitCode(err) == 1 && stderrOf(err) == "" {
			return nil, nil // no match
		}
		return nil, mapNotFound(err, rev)
	}
	var paths []string
	for _, p := range splitNUL(out) {
		// Matches are printed as "<rev>:<path>".
		if rest, ok := strings.CutPrefix(p, rev+":"); ok {
			p = rest
		} else if rest, ok := strings.CutPrefix(p, rev+"/"); ok && strings.Contains(rev, ":") {
			p = rest
		}
		paths = append(paths, p)
		if len(paths) == max {
			break
		}
	}
	return paths, nil
}
