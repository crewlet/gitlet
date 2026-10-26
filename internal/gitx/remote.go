package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (r *Repo) fetch(ctx context.Context, opt FetchOptions) error {
	if err := checkRemote(opt.Remote); err != nil {
		return err
	}
	for _, rs := range opt.Refspecs {
		if rs == "" || strings.ContainsAny(rs, "\x00\n ") {
			return fmt.Errorf("gitx: invalid refspec %q", rs)
		}
	}
	var args []string
	if opt.Metadata {
		args = append(args, "-c", "fetch.pruneTags=false")
	}
	args = append(args, "fetch")
	if opt.Prune {
		args = append(args, "--prune")
	}
	if opt.Metadata {
		args = append(args, "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
			"--no-show-forced-updates", "--no-auto-maintenance")
		if len(opt.Refspecs) > 0 {
			// Without it, fetching refs/heads/main would also move the user's
			// refs/remotes/<remote>/main through the configured refspec.
			args = append(args, "--refmap=")
		}
	}
	args = append(args, "--", opt.Remote)
	args = append(args, opt.Refspecs...)
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = timeoutFetch
	}
	_, err := r.exec(ctx, cmdOpts{timeout: timeout}, args...)
	// An exact ref the remote lacks (a deleted claim or wip, a main not yet
	// pushed) fails the whole fetch: report it as ErrNotFound.
	if err != nil && strings.Contains(stderrOf(err), "couldn't find remote ref") {
		return fmt.Errorf("gitx: fetch from %s: %w (%w)", opt.Remote, ErrNotFound, err)
	}
	return err
}

func (r *Repo) push(ctx context.Context, opt PushOptions) (PushReport, error) {
	if err := checkRemote(opt.Remote); err != nil {
		return PushReport{}, err
	}
	if len(opt.Updates) == 0 {
		return PushReport{}, nil
	}
	var args []string
	if opt.Metadata {
		args = append(args, "-c", "push.negotiate=false", "-c", "push.gpgSign=false")
	}
	// One lease per ref, each judged on its own (docs/SPEC.md, section 6.1).
	args = append(args, "push", "--porcelain", "--no-atomic")
	if opt.Metadata {
		// --no-follow-tags keeps push.followTags from sending the user's
		// tags along with snapshot and claim commits.
		args = append(args, "--no-verify", "--no-recurse-submodules", "--no-follow-tags")
	}
	refspecs := make([]string, 0, len(opt.Updates))
	for _, u := range opt.Updates {
		lease, spec, err := pushSpec(u)
		if err != nil {
			return PushReport{}, err
		}
		if lease != "" {
			args = append(args, lease)
		}
		refspecs = append(refspecs, spec)
	}
	args = append(args, "--", opt.Remote)
	args = append(args, refspecs...)
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = timeoutPush
	}

	stdout, stderr, err := r.execFull(ctx, cmdOpts{timeout: timeout}, args...)
	report := PushReport{Remote: remoteLines(string(stderr))}
	report.Results = parsePorcelain(string(stdout), report.Remote)
	if err != nil {
		// A rejected ref makes git exit non-zero, but it is a result, not a
		// failure. No result at all means git never got that far.
		if len(report.Results) == 0 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return report, err
		}
	}
	return report, nil
}

// pushSpec returns the lease option and the refspec for one update. Every
// lease names its expected value explicitly: the bare --force-with-lease=<ref>
// form would trust the local remote-tracking ref instead.
func pushSpec(u PushUpdate) (lease, spec string, err error) {
	for _, s := range []string{u.Src, u.Dst, u.OldOID} {
		if strings.ContainsAny(s, ": \t\n\x00") {
			return "", "", fmt.Errorf("gitx: invalid push update %+v", u)
		}
	}
	if u.Dst == "" || strings.HasPrefix(u.Src, "+") || strings.HasPrefix(u.Dst, "-") {
		return "", "", fmt.Errorf("gitx: invalid push update %+v", u)
	}
	spec = u.Src + ":" + u.Dst // an empty Src deletes Dst
	switch u.Expect {
	case ExpectFastForward:
	case ExpectAbsent:
		lease = "--force-with-lease=" + u.Dst + ":"
	case ExpectOID:
		old := u.OldOID
		if old == ZeroOID {
			old = "" // must not exist
		}
		lease = "--force-with-lease=" + u.Dst + ":" + old
	case Force:
		if u.Src != "" {
			spec = "+" + spec
		}
	default:
		return "", "", fmt.Errorf("gitx: unknown push expectation %d", u.Expect)
	}
	return lease, spec, nil
}

// parsePorcelain reads git push --porcelain output:
//
//	To <url>
//	<flag> TAB <src>:<dst> TAB <summary> [(<reason>)]
//	Done
//
// remote is the remote's stderr, used to recognise a lease the remote
// itself refused under its ref lock.
func parsePorcelain(out string, remote []string) []PushResult {
	var results []PushResult
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 || len(f[0]) != 1 {
			continue
		}
		dst := f[1][strings.LastIndex(f[1], ":")+1:]
		summary, reason := f[2], ""
		if i := strings.Index(summary, " ("); i >= 0 && strings.HasSuffix(summary, ")") {
			summary, reason = summary[:i], summary[i+2:len(summary)-1]
		}
		res := PushResult{Dst: dst}
		switch f[0][0] {
		case '=':
			res.OK, res.UpToDate = true, true
		case ' ', '+', '-', '*':
			res.OK = true
		default: // '!' rejected, 'X' no match
			res.Kind = rejectKind(summary, reason, dst, remote)
			res.Reason = reason
			if res.Reason == "" {
				res.Reason = strings.Trim(summary, "[]")
			}
		}
		results = append(results, res)
	}
	return results
}

func rejectKind(summary, reason, dst string, remote []string) RejectKind {
	switch summary {
	case "[rejected]":
		switch reason {
		case "stale info", "remote ref updated since checkout":
			return RejectStale
		case "non-fast-forward", "fetch first", "needs force", "already exists":
			return RejectNonFF
		}
	case "[remote rejected]":
		// The client checks leases against the refs the remote advertised;
		// a push that lost a race after that is refused by the remote's ref
		// transaction, which is the same failed lease.
		if containsAny(strings.ToLower(reason), casFailures) {
			return RejectStale
		}
		if reason == "failed to update ref" || reason == "failed to lock" {
			for _, l := range remote {
				if ll := strings.ToLower(l); strings.Contains(ll, "cannot lock ref '"+strings.ToLower(dst)+"'") {
					return RejectStale
				}
			}
		}
		return RejectRemote
	}
	return RejectOther
}

// casFailures are reasons remotes give when a ref's old value did not match.
var casFailures = []string{
	"cannot lock ref",
	"incorrect old value",
	"but expected",
	"reference already exists",
	"stale info",
}

// remoteLines returns the remote's messages from stderr, without the
// "remote:" prefix and the padding git adds to erase progress output.
func remoteLines(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		for _, seg := range strings.Split(line, "\r") {
			rest, ok := strings.CutPrefix(seg, "remote:")
			if !ok {
				continue
			}
			rest = strings.ReplaceAll(rest, "\x1b[K", "")
			rest = strings.TrimRight(strings.TrimPrefix(rest, " "), " \t")
			if rest != "" {
				out = append(out, rest)
			}
		}
	}
	return out
}
