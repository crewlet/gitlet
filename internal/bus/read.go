package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// stampFile sits next to fetch.lock and records the last successful poll of
// any process of the clone, so others can reuse it.
const stampFile = "fetch.stamp"

type fetchStamp struct {
	Remote      string    `json:"remote"`
	At          time.Time `json:"at"`
	Fingerprint string    `json:"fingerprint"`
}

func (b *Bus) fetch(ctx context.Context) (FetchResult, error) {
	if b.remoteName == "" {
		fp, err := b.fingerprint(ctx)
		if err != nil {
			return FetchResult{}, fmt.Errorf("bus: fetch: %w", err)
		}
		return FetchResult{At: b.now(), Fingerprint: fp}, nil
	}
	if st, ok := b.freshStamp(); ok {
		return b.reuseFetch(ctx, st)
	}
	var res FetchResult
	err := b.withFetchLock(ctx, func() error {
		// Another process may have fetched while we waited for the lock.
		if st, ok := b.freshStamp(); ok {
			var err error
			res, err = b.reuseFetch(ctx, st)
			return err
		}
		at := b.now()
		if err := b.fetchMetadata(ctx); err != nil {
			return err
		}
		fp, err := b.fingerprint(ctx)
		if err != nil {
			return err
		}
		b.writeStamp(fetchStamp{Remote: b.remoteName, At: at, Fingerprint: fp})
		res = FetchResult{At: at, Fingerprint: fp}
		return nil
	})
	if err != nil {
		return FetchResult{}, fmt.Errorf("bus: fetch: %w", err)
	}
	return res, nil
}

// fetchMetadata runs the poll's fetch (docs/SPEC.md, section 6.2). A remote
// without the main branch yet is polled without it.
func (b *Bus) fetchMetadata(ctx context.Context) error {
	specs := []string{
		"+" + AgentsPrefix + "*:" + b.tracking(AgentsPrefix) + "*",
		"+" + ClaimsPrefix + "*:" + b.tracking(ClaimsPrefix) + "*",
		"+refs/heads/" + b.mainBranch + ":" + b.mainRef(),
	}
	opt := gitx.FetchOptions{Remote: b.remoteName, Refspecs: specs, Prune: true, Metadata: true}
	err := b.repo.Fetch(ctx, opt)
	if isMissingRemoteRef(err) {
		opt.Refspecs = specs[:2]
		err = b.repo.Fetch(ctx, opt)
	}
	return err
}

// reuseFetch answers a poll with a recent fetch of another process. The
// fingerprint is computed again because own pushes since then may have
// moved tracking refs.
func (b *Bus) reuseFetch(ctx context.Context, st fetchStamp) (FetchResult, error) {
	fp, err := b.fingerprint(ctx)
	if err != nil {
		return FetchResult{}, fmt.Errorf("bus: fetch: %w", err)
	}
	return FetchResult{At: st.At, Reused: true, Fingerprint: fp}, nil
}

// freshStamp returns the last poll of this clone when it is newer than
// FetchReuse. Clock jumps that put the stamp in the future make it stale.
func (b *Bus) freshStamp() (fetchStamp, bool) {
	if b.lockDir == "" || b.fetchReuse < 0 {
		return fetchStamp{}, false
	}
	data, err := os.ReadFile(filepath.Join(b.lockDir, stampFile))
	if err != nil {
		return fetchStamp{}, false
	}
	var st fetchStamp
	if json.Unmarshal(data, &st) != nil || st.Remote != b.remoteName {
		return fetchStamp{}, false
	}
	age := b.now().Sub(st.At)
	return st, age >= 0 && age < b.fetchReuse
}

// writeStamp records a successful poll, atomically. The stamp only saves
// work for other processes, so failing to write it is ignored.
func (b *Bus) writeStamp(st fetchStamp) {
	if b.lockDir == "" {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(b.lockDir, stampFile+".*")
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), filepath.Join(b.lockDir, stampFile)) != nil {
		os.Remove(f.Name())
	}
}

// fingerprint hashes the refs whose change means the shared state must be
// reloaded: the tracking refs of agents and claims, gitlet's main ref, and
// this agent's own local documents (which a CLI process may have written).
func (b *Bus) fingerprint(ctx context.Context) (string, error) {
	prefixes := []string{b.tracking(AgentsPrefix), b.tracking(ClaimsPrefix)}
	if b.remoteName != "" {
		prefixes = append(prefixes, AgentsPrefix+string(b.me)+"/")
	}
	lines := map[string]string{}
	for _, p := range prefixes {
		refs, err := b.repo.ListRefs(ctx, p)
		if err != nil {
			return "", err
		}
		for name, oid := range refs {
			lines[name] = oid
		}
	}
	main, err := b.localOID(ctx, b.mainRef())
	if err != nil {
		return "", err
	}
	lines[b.mainRef()] = main
	h := sha256.New()
	for _, name := range sortedKeys(lines) {
		fmt.Fprintf(h, "%s %s\n", name, lines[name])
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

func (b *Bus) fetchOutside(ctx context.Context, window time.Duration, max int) ([]gitx.RefInfo, error) {
	if b.remoteName == "" {
		// Nobody pushes branches without a remote.
		return nil, nil
	}
	prefix := headsPrefix + b.remoteName + "/"
	err := b.withFetchLock(ctx, func() error {
		return b.repo.Fetch(ctx, gitx.FetchOptions{
			Remote:   b.remoteName,
			Refspecs: []string{"+refs/heads/*:" + prefix + "*"},
			Metadata: true,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("bus: fetch branches: %w", err)
	}
	infos, err := b.repo.ListRefsByDate(ctx, prefix, 0)
	if err != nil {
		return nil, fmt.Errorf("bus: fetch branches: %w", err)
	}
	sort.SliceStable(infos, func(i, j int) bool {
		if !infos[i].CommitterDate.Equal(infos[j].CommitterDate) {
			return infos[i].CommitterDate.After(infos[j].CommitterDate)
		}
		return infos[i].Name < infos[j].Name
	})
	cutoff := b.now().Add(-window)
	var out []gitx.RefInfo
	for _, ri := range infos {
		branch := strings.TrimPrefix(ri.Name, prefix)
		if branch == b.mainBranch || (window > 0 && ri.CommitterDate.Before(cutoff)) {
			continue
		}
		// Report the branch under its name on the remote.
		ri.Name = "refs/heads/" + branch
		out = append(out, ri)
		if max > 0 && len(out) == max {
			break
		}
	}
	return out, nil
}

func (b *Bus) fetchWip(ctx context.Context, agent model.AgentID, item model.ItemID, want string) (string, error) {
	switch {
	case !model.ValidAgentID(agent):
		return "", fmt.Errorf("bus: fetch wip: invalid agent id %q", agent)
	case !model.ValidItemID(item):
		return "", fmt.Errorf("bus: fetch wip: invalid item id %q", item)
	case !validOID(want):
		return "", fmt.Errorf("bus: fetch wip: invalid snapshot %q", want)
	}
	ref := WipRef(agent, item)
	tr := b.tracking(ref)
	have, err := b.localOID(ctx, tr)
	if err != nil {
		return "", fmt.Errorf("bus: fetch %s: %w", ref, err)
	}
	if have == want {
		return want, nil
	}
	if b.remoteName == "" {
		if have == "" {
			return "", fmt.Errorf("bus: fetch %s: %w", ref, gitx.ErrNotFound)
		}
		return have, nil
	}
	// A peer working in another worktree of this clone has it locally.
	if local, err := b.localOID(ctx, ref); err == nil && local == want {
		return want, nil
	}
	got, err := b.fetchExact(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("bus: fetch %s: %w", ref, err)
	}
	if got == "" {
		return "", fmt.Errorf("bus: fetch %s: not on the remote: %w", ref, gitx.ErrNotFound)
	}
	return got, nil
}

func (b *Bus) pruneWips(ctx context.Context, keep map[string]bool) error {
	if b.remoteName == "" {
		// The local refs are the peers' own refs, not copies.
		return nil
	}
	refs, err := b.listCanonical(ctx, WipPrefix, true)
	if err != nil {
		return fmt.Errorf("bus: prune wips: %w", err)
	}
	var errs []error
	for _, ref := range sortedKeys(refs) {
		if keep[ref] || strings.HasPrefix(ref, WipPrefix+string(b.me)+"/") {
			continue
		}
		if err := b.repo.DeleteRef(ctx, b.tracking(ref), refs[ref]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("bus: prune wips: %w", err)
	}
	return nil
}

// docRef is a parsed agents/ or claims/ ref.
type docRef struct {
	kind  string // "state", "outbox" or "claim"
	agent model.AgentID
	key   string
}

// file is the name of the document in the ref's commit.
func (d docRef) file() string {
	switch d.kind {
	case "state":
		return stateFile
	case "outbox":
		return outboxFile
	}
	return claimFile
}

// parseDocRef parses a canonical ref, or returns why it is not a document
// ref gitlet knows.
func parseDocRef(ref string) (d docRef, reason string) {
	if rest, ok := strings.CutPrefix(ref, AgentsPrefix); ok {
		agent, kind, ok := strings.Cut(rest, "/")
		if !ok || (kind != "state" && kind != "outbox") {
			return d, "unknown agents ref"
		}
		if !model.ValidAgentID(model.AgentID(agent)) {
			return d, fmt.Sprintf("invalid agent id %q", agent)
		}
		return docRef{kind: kind, agent: model.AgentID(agent)}, ""
	}
	if key, ok := strings.CutPrefix(ref, ClaimsPrefix); ok {
		if !validKey(key) {
			return d, fmt.Sprintf("invalid symbol key %q", key)
		}
		return docRef{kind: "claim", key: key}, ""
	}
	return d, "unknown ref"
}

func (b *Bus) load(ctx context.Context) (*State, error) {
	st := &State{
		Agents:   map[model.AgentID]model.AgentState{},
		Outboxes: map[model.AgentID]model.Outbox{},
		Claims:   map[string]ClaimView{},
		Refs:     map[string]string{},
	}
	tracking := b.remoteName != ""
	for _, prefix := range []string{AgentsPrefix, ClaimsPrefix} {
		refs, err := b.listCanonical(ctx, prefix, tracking)
		if err != nil {
			return nil, fmt.Errorf("bus: load: %w", err)
		}
		for ref, oid := range refs {
			st.Refs[ref] = oid
		}
	}
	if tracking {
		// Own documents come from the local refs, never older than the
		// remote copies.
		for _, ref := range []string{StateRef(b.me), OutboxRef(b.me)} {
			delete(st.Refs, ref)
			oid, err := b.localOID(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("bus: load: %w", err)
			}
			if oid != "" {
				st.Refs[ref] = oid
			}
		}
	}
	main, err := b.localOID(ctx, b.mainRef())
	if err != nil {
		return nil, fmt.Errorf("bus: load: %w", err)
	}
	st.Main = main

	var rd objectReader
	defer func() {
		if rd != nil {
			rd.Close()
		}
	}()
	for _, ref := range sortedKeys(st.Refs) {
		d, reason := parseDocRef(ref)
		if reason != "" {
			st.Skipped = append(st.Skipped, ref+": "+reason)
			continue
		}
		if rd == nil {
			if rd, err = b.openReader(ctx); err != nil {
				return nil, fmt.Errorf("bus: load: %w", err)
			}
		}
		file := d.file()
		oid := st.Refs[ref]
		data, size, ok, err := rd.ReadObject(oid+":"+file, MaxReadDoc)
		switch {
		case errors.Is(err, gitx.ErrNotFound):
			st.Skipped = append(st.Skipped, fmt.Sprintf("%s: commit %s has no %s", ref, oid, file))
			continue
		case err != nil:
			return nil, fmt.Errorf("bus: load %s: %w", ref, err)
		case !ok:
			st.Skipped = append(st.Skipped, fmt.Sprintf("%s: too large (%d bytes, limit %d)", ref, size, MaxReadDoc))
			continue
		}
		if err := st.add(ref, d, oid, data); err != nil {
			st.Skipped = append(st.Skipped, ref+": "+err.Error())
		}
	}
	return st, nil
}

// add decodes and validates one document, read from ref, into the state.
// Footprint entries with invalid paths are skipped on their own.
func (st *State) add(ref string, d docRef, oid string, data []byte) error {
	switch d.kind {
	case "state":
		doc, dropped, err := decodeState(d.agent, data)
		if err != nil {
			return err
		}
		for _, s := range dropped {
			st.Skipped = append(st.Skipped, ref+": "+s)
		}
		st.Agents[d.agent] = doc
	case "outbox":
		doc, err := decodeOutbox(d.agent, data)
		if err != nil {
			return err
		}
		st.Outboxes[d.agent] = doc
	case "claim":
		doc, err := decodeClaim(d.key, data)
		if err != nil {
			return err
		}
		st.Claims[d.key] = ClaimView{Claim: doc, OID: oid}
	}
	return nil
}
