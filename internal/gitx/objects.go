package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// noSign is prepended to every commit gitlet writes itself.
var noSign = []string{"-c", "commit.gpgSign=false"}

func (r *Repo) hashBlob(ctx context.Context, data []byte) (string, error) {
	if data == nil {
		data = []byte{}
	}
	// Content read from stdin is hashed as is, without attribute filters.
	out, err := r.exec(ctx, cmdOpts{stdin: data}, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) makeTree(ctx context.Context, entries []TreeEntry) (string, error) {
	var in strings.Builder
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\x00") {
			return "", fmt.Errorf("gitx: mktree: invalid entry name %q", e.Name)
		}
		if seen[e.Name] {
			return "", fmt.Errorf("gitx: mktree: duplicate entry %q", e.Name)
		}
		seen[e.Name] = true
		for _, tok := range []string{e.Mode, e.Type, e.OID} {
			if tok == "" || strings.ContainsAny(tok, " \t\n\x00") {
				return "", fmt.Errorf("gitx: mktree: invalid entry %+v", e)
			}
		}
		fmt.Fprintf(&in, "%s %s %s\t%s\x00", e.Mode, e.Type, e.OID, e.Name)
	}
	out, err := r.exec(ctx, cmdOpts{stdin: []byte(in.String())}, "mktree", "-z")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) commitTree(ctx context.Context, tree string, parents []string, message string, id Identity) (string, error) {
	if err := checkRev(tree); err != nil {
		return "", err
	}
	args := slices.Concat(noSign, []string{"commit-tree", tree})
	for _, p := range parents {
		if err := checkRev(p); err != nil {
			return "", err
		}
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	return r.commitWith(ctx, args, nil, message, id)
}

// commitWith runs a commit-tree command with the message on stdin and id as
// author and committer. A zero part of id is left to git; when git has no
// identity of its own, FallbackIdentity fills the gaps.
func (r *Repo) commitWith(ctx context.Context, args, env []string, message string, id Identity) (string, error) {
	if message != "" && !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	idEnv, err := identityEnv(id)
	if err != nil {
		return "", err
	}
	run := func(idEnv []string) (string, error) {
		out, err := r.exec(ctx, cmdOpts{stdin: []byte(message), env: slices.Concat(env, idEnv)}, args...)
		return strings.TrimSpace(string(out)), err
	}
	oid, err := run(idEnv)
	if err != nil && isIdentityError(err) {
		fb := id
		if fb.Name == "" {
			fb.Name = FallbackIdentity.Name
		}
		if fb.Email == "" {
			fb.Email = FallbackIdentity.Email
		}
		if idEnv, ferr := identityEnv(fb); ferr == nil {
			oid, err = run(idEnv)
		}
	}
	return oid, err
}

// identityEnv sets author and committer from the non-zero parts of id.
func identityEnv(id Identity) ([]string, error) {
	for _, s := range []string{id.Name, id.Email} {
		if strings.ContainsAny(s, "<>\n\x00") {
			return nil, fmt.Errorf("gitx: invalid identity %q", s)
		}
	}
	var env []string
	if id.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+id.Name, "GIT_COMMITTER_NAME="+id.Name)
	}
	if id.Email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+id.Email, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	if !id.When.IsZero() {
		date := "@" + strconv.FormatInt(id.When.Unix(), 10) + " " + id.When.Format("-0700")
		env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	return env, nil
}

var identityPatterns = []string{
	"identity unknown",
	"please tell me who you are",
	"unable to auto-detect email address",
	"no email was given",
	"no name was given",
	"empty ident name",
	"name consists only of disallowed characters",
}

func isIdentityError(err error) bool { return containsAny(stderrOf(err), identityPatterns) }

func (r *Repo) commitFile(ctx context.Context, base, path string, content []byte, message string, id Identity) (string, error) {
	if err := checkTreePath(path); err != nil {
		return "", err
	}
	baseOID, err := r.revParse(ctx, base+"^{commit}")
	if err != nil {
		return "", err
	}
	blob, err := r.hashBlob(ctx, content)
	if err != nil {
		return "", err
	}

	dir, err := r.scratchDir(ctx)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, "commit-")
	if err != nil {
		return "", fmt.Errorf("gitx: commit file: %w", err)
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	idx := func(stdin []byte, args ...string) (string, error) {
		args = append([]string{"-c", "core.splitIndex=false", "--literal-pathspecs"}, args...)
		out, err := r.exec(ctx, cmdOpts{stdin: stdin, env: env}, args...)
		return string(out), err
	}

	if _, err := idx(nil, "read-tree", baseOID); err != nil {
		return "", err
	}
	// Keep the executable bit of a file that already exists.
	mode := "100644"
	if out, err := idx(nil, "ls-files", "-s", "-z", "--", path); err != nil {
		return "", err
	} else if strings.HasPrefix(out, "100755 ") {
		mode = "100755"
	}
	if _, err := idx(nil, "update-index", "--add", "--replace", "--cacheinfo", mode+","+blob+","+path); err != nil {
		return "", err
	}
	tree, err := idx(nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := slices.Concat(noSign, []string{"commit-tree", strings.TrimSpace(tree), "-p", baseOID, "-F", "-"})
	return r.commitWith(ctx, args, nil, message, id)
}

// checkTreePath accepts a clean relative slash path that git can store.
func checkTreePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("gitx: invalid path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("gitx: invalid path %q", p)
		}
	}
	return nil
}

// scratchDir returns <git dir>/gitlet, where temporary index files live,
// creating it when missing.
func (r *Repo) scratchDir(ctx context.Context) (string, error) {
	gd, err := r.gitDir(ctx)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(gd, "gitlet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("gitx: %w", err)
	}
	return dir, nil
}

func (r *Repo) updateRef(ctx context.Context, ref, newOID, oldOID string) error {
	if err := errors.Join(checkName("ref", ref), checkRev(newOID)); err != nil {
		return err
	}
	args := []string{"update-ref", ref, newOID}
	if oldOID != "" {
		if err := checkRev(oldOID); err != nil {
			return err
		}
		args = append(args, oldOID) // ZeroOID: the ref must not exist
	}
	_, err := r.exec(ctx, cmdOpts{}, args...)
	return err
}

func (r *Repo) deleteRef(ctx context.Context, ref, oldOID string) error {
	if err := checkName("ref", ref); err != nil {
		return err
	}
	args := []string{"update-ref", "-d", ref}
	if oldOID != "" {
		if err := checkRev(oldOID); err != nil {
			return err
		}
		args = append(args, oldOID)
	}
	_, err := r.exec(ctx, cmdOpts{}, args...)
	return err
}
