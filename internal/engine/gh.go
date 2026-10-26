package engine

// Pull requests for submit (docs/SPEC.md, section 12.4): gh opens one when
// it is installed and signed in; otherwise the forge's compare page is the
// way to open one by hand.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ghCreateTimeout bounds gh pr create, which waits on the forge's API.
const ghCreateTimeout = 30 * time.Second

// prRequest is a pull request to open.
type prRequest struct {
	Branch, Base string
	Title, Body  string
	Draft        bool
}

// prResult is what came of a prRequest.
type prResult struct {
	URL      string // the pull request gh opened or found
	Existing bool   // it was already open
	Compare  string // the forge's page for opening it by hand, when gh did not
	Warning  string // why gh did not open it, when gh was tried
}

// pullRequest opens a pull request for req.Branch through gh, or returns
// the forge's compare URL. gh is tried unless the remote is on a forge gh
// does not serve, so GitHub Enterprise hosts with any name work; its
// failure is a warning on a known GitHub host only. It never fails submit.
func (e *Engine) pullRequest(ctx context.Context, req prRequest) prResult {
	raw, err := e.repo.RemoteURL(ctx, e.bus.Remote())
	if err != nil {
		e.log.Debug("remote url", "err", err)
		raw = ""
	}
	f := parseForge(raw)
	var res prResult
	if f.kind == "" || f.kind == forgeGitHub {
		if gh, err := exec.LookPath("gh"); err == nil {
			u, existing, err := e.ghPullRequest(ctx, gh, req)
			switch {
			case err == nil:
				return prResult{URL: u, Existing: existing}
			case f.kind == forgeGitHub:
				res.Warning = "gh could not open the pull request: " + err.Error()
			default:
				e.log.Debug("gh pr", "err", err)
			}
		}
	}
	res.Compare = f.compareURL(req.Base, req.Branch)
	return res
}

// ghPullRequest returns the open pull request of req.Branch, or opens one.
func (e *Engine) ghPullRequest(ctx context.Context, gh string, req prRequest) (u string, existing bool, err error) {
	if out, err := e.runGh(ctx, gh, ghTimeout, "pr", "view", req.Branch, "--json", "url,state"); err == nil {
		var pr struct {
			URL   string `json:"url"`
			State string `json:"state"`
		}
		if json.Unmarshal(out, &pr) == nil && pr.URL != "" && strings.EqualFold(pr.State, "OPEN") {
			return pr.URL, true, nil
		}
	}
	args := []string{"pr", "create", "--base", req.Base, "--head", req.Branch, "--title", req.Title, "--body", req.Body}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, err := e.runGh(ctx, gh, ghCreateTimeout, args...)
	if err != nil {
		return "", false, err
	}
	if u = lastURL(string(out)); u == "" {
		return "", false, errors.New("gh printed no pull request URL")
	}
	return u, false, nil
}

// runGh runs gh in the repository without prompts, within timeout. Its
// error says what gh said.
func (e *Engine) runGh(ctx context.Context, gh string, timeout time.Duration, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, gh, args...)
	cmd.Dir = e.repo.Root
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1")
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case err == nil:
		return out, nil
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return nil, fmt.Errorf("gh %s %s timed out after %s", args[0], args[1], timeout)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	if reason := firstLine(strings.TrimSpace(stderr.String())); reason != "" {
		return nil, errors.New(reason)
	}
	return nil, err
}

// lastURL returns the last line of out that is a URL: gh pr create prints
// the new pull request's.
func lastURL(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l
		}
	}
	return ""
}

// Forges with a known compare page.
const (
	forgeGitHub    = "github"
	forgeGitLab    = "gitlab"
	forgeBitbucket = "bitbucket"
)

// forge is the web side of a remote.
type forge struct {
	kind string // forgeGitHub, forgeGitLab, forgeBitbucket, or "" when unknown
	web  string // the repository's web page, as https://host/owner/repo
}

// parseForge recognizes the forge of a remote URL in any of git's forms:
// https://host/owner/repo.git, ssh://git@host/owner/repo.git and the scp
// form git@host:owner/repo.git. Self-hosted GitHub and GitLab are known by
// a host that starts with "github." or "gitlab.".
func parseForge(raw string) forge {
	scheme, host, path, ok := splitRemoteURL(raw)
	if !ok {
		return forge{}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !strings.Contains(path, "/") {
		return forge{}
	}
	var f forge
	switch h := strings.ToLower(host); {
	case h == "github.com" || h == "www.github.com" || h == "ssh.github.com":
		f.kind, host = forgeGitHub, "github.com"
	case h == "gitlab.com" || h == "altssh.gitlab.com":
		f.kind, host = forgeGitLab, "gitlab.com"
	case h == "bitbucket.org" || h == "altssh.bitbucket.org":
		f.kind, host = forgeBitbucket, "bitbucket.org"
	case strings.HasPrefix(h, "github."):
		f.kind = forgeGitHub
	case strings.HasPrefix(h, "gitlab."):
		f.kind = forgeGitLab
	default:
		return forge{}
	}
	f.web = scheme + "://" + host + "/" + path
	return f
}

// splitRemoteURL splits a network remote URL into the scheme its web page
// uses, its host and its repository path. Local paths and file:// URLs are
// not network remotes.
func splitRemoteURL(raw string) (scheme, host, path string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", false
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", "", "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http":
			// The web page is on the same host and port.
			return strings.ToLower(u.Scheme), u.Host, u.Path, true
		case "ssh", "git", "git+ssh", "ssh+git":
			// The SSH port says nothing about the web's.
			return "https", u.Hostname(), u.Path, true
		}
		return "", "", "", false
	}
	// The scp form, [user@]host:path, as git reads it: a slash before the
	// first colon makes a local path, and so does a one-letter drive name.
	colon := strings.IndexByte(raw, ':')
	if colon < 0 || strings.Contains(raw[:colon], "/") {
		return "", "", "", false
	}
	host = raw[:colon]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if len(host) < 2 {
		return "", "", "", false
	}
	return "https", host, raw[colon+1:], true
}

// compareURL returns the page that opens a pull request of branch into
// base ("" for an unknown forge).
func (f forge) compareURL(base, branch string) string {
	switch f.kind {
	case forgeGitHub:
		return f.web + "/compare/" + escapeRefPath(base) + "..." + escapeRefPath(branch) + "?expand=1"
	case forgeGitLab:
		q := url.Values{}
		q.Set("merge_request[source_branch]", branch)
		q.Set("merge_request[target_branch]", base)
		return f.web + "/-/merge_requests/new?" + q.Encode()
	case forgeBitbucket:
		q := url.Values{}
		q.Set("source", branch)
		q.Set("dest", base)
		return f.web + "/pull-requests/new?" + q.Encode()
	}
	return ""
}

// escapeRefPath escapes a branch name for a URL path, keeping its slashes.
func escapeRefPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
