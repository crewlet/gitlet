package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// refPattern turns a string prefix into a for-each-ref pattern. for-each-ref
// matches whole path components, so the pattern stops at the prefix's last
// slash and the caller filters on the full prefix.
func refPattern(prefix string) []string {
	i := strings.LastIndex(prefix, "/")
	if i < 0 {
		return nil
	}
	return []string{prefix[:i+1]}
}

func (r *Repo) listRefs(ctx context.Context, prefix string) (map[string]string, error) {
	if strings.HasPrefix(prefix, "-") || strings.ContainsAny(prefix, "\x00\n") {
		return nil, fmt.Errorf("gitx: invalid ref prefix %q", prefix)
	}
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, refPattern(prefix)...)
	out, err := r.read(ctx, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, l := range lines(out) {
		oid, name, ok := strings.Cut(l, " ")
		if ok && strings.HasPrefix(name, prefix) {
			refs[name] = oid
		}
	}
	return refs, nil
}

func (r *Repo) listRefsByDate(ctx context.Context, prefix string, max int) ([]RefInfo, error) {
	if strings.HasPrefix(prefix, "-") || strings.ContainsAny(prefix, "\x00\n") {
		return nil, fmt.Errorf("gitx: invalid ref prefix %q", prefix)
	}
	// The last --sort key is the primary one: newest first, then by name.
	args := append([]string{"for-each-ref", "--sort=refname", "--sort=-committerdate",
		"--format=%(objectname) %(objecttype) %(committerdate:unix) %(refname)"}, refPattern(prefix)...)
	out, err := r.read(ctx, args...)
	if err != nil {
		return nil, err
	}
	var list []RefInfo
	for _, l := range lines(out) {
		f := strings.SplitN(l, " ", 4)
		if len(f) != 4 || f[1] != "commit" || !strings.HasPrefix(f[3], prefix) {
			continue
		}
		sec, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("gitx: for-each-ref: bad date in %q", l)
		}
		list = append(list, RefInfo{Name: f[3], OID: f[0], CommitterDate: time.Unix(sec, 0)})
		if max > 0 && len(list) == max {
			break
		}
	}
	return list, nil
}

func (r *Repo) lsRemote(ctx context.Context, remote string, heads bool, patterns ...string) (map[string]string, error) {
	if err := checkRemote(remote); err != nil {
		return nil, err
	}
	args := []string{"ls-remote"}
	if heads {
		// Sent to the server as a ref prefix, unlike patterns, which
		// filter what it lists.
		args = append(args, "--heads")
	}
	for _, p := range patterns {
		if err := checkName("ref pattern", p); err != nil {
			return nil, err
		}
	}
	args = append(append(args, remote), patterns...)
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutFetch}, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, l := range lines(string(out)) {
		oid, name, ok := strings.Cut(l, "\t")
		if ok && !strings.HasSuffix(name, "^{}") {
			refs[name] = oid
		}
	}
	return refs, nil
}
