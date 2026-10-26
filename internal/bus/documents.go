package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// errNewerFormat marks a document written by a newer gitlet.
var errNewerFormat = errors.New("newer format")

var (
	oidPattern      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	keyPattern      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	hex12Pattern    = regexp.MustCompile(`^[0-9a-f]{12}$`)
	threadPattern   = regexp.MustCompile(`^t-[0-9a-f]{12}$`)
	proposalPattern = regexp.MustCompile(`^p-[0-9a-f]{12}$`)
)

// validOID reports whether s is a full object id (SHA-1 or SHA-256).
func validOID(s string) bool { return oidPattern.MatchString(s) }

// validOptOID is validOID that also accepts "".
func validOptOID(s string) bool { return s == "" || validOID(s) }

// validKey reports whether s is a symbol key (docs/SPEC.md, section 5).
func validKey(s string) bool { return keyPattern.MatchString(s) }

// validPath reports whether p is a clean relative slash path inside the
// repository: not empty, not absolute, no "." or ".." segments, no control
// characters.
func validPath(p string) bool {
	if p == "" || len(p) > 4096 || !utf8.ValidString(p) || hasControl(p) {
		return false
	}
	if strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// validBranch reports whether s is a branch name git would accept (the
// rules of git check-ref-format that matter for safety), or "".
func validBranch(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 255 || hasControl(s) || strings.ContainsAny(s, " ~^:?*[\\") {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") ||
		strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") || s == "@" {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// checker records the first problem found in a document.
type checker struct{ err error }

func (c *checker) require(ok bool, format string, args ...any) {
	if !ok && c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

func validStatus(s model.ItemStatus) bool {
	switch s {
	case model.ItemActive, model.ItemSubmitted, model.ItemLanded, model.ItemAbandoned:
		return true
	}
	return false
}

// validateState checks every id and path in the state document of agent.
func validateState(agent model.AgentID, st *model.AgentState) error {
	var c checker
	c.require(st.Agent.ID == agent, "agent id %q does not match the ref", st.Agent.ID)
	c.require(st.LeaseSeconds >= 0, "negative leaseSeconds")
	seen := make(map[model.ItemID]bool, len(st.Items))
	for i := range st.Items {
		it := &st.Items[i]
		c.require(model.ValidItemID(it.ID), "items[%d]: invalid item id %q", i, it.ID)
		c.require(!seen[it.ID], "items[%d]: duplicate item id %q", i, it.ID)
		seen[it.ID] = true
		c.require(validStatus(it.Status), "items[%d]: invalid status %q", i, it.Status)
		c.require(validBranch(it.Branch), "items[%d]: invalid branch %q", i, it.Branch)
		c.require(validOptOID(it.Base), "items[%d]: invalid base %q", i, it.Base)
		c.require(validOptOID(it.Snapshot), "items[%d]: invalid snapshot %q", i, it.Snapshot)
		c.require(validOptOID(it.Head), "items[%d]: invalid head %q", i, it.Head)
		checkFootprint(&c, i, &it.Footprint)
	}
	return c.err
}

func checkFootprint(c *checker, i int, fp *model.Footprint) {
	c.require(validOptOID(fp.Base), "items[%d].footprint: invalid base %q", i, fp.Base)
	c.require(validOptOID(fp.Head), "items[%d].footprint: invalid head %q", i, fp.Head)
	for j, f := range fp.Files {
		c.require(validPath(f.Path), "items[%d].footprint.files[%d]: invalid path %q", i, j, f.Path)
		c.require(f.OldPath == "" || validPath(f.OldPath), "items[%d].footprint.files[%d]: invalid oldPath %q", i, j, f.OldPath)
	}
	for j := range fp.Symbols {
		s := &fp.Symbols[j]
		c.require(validKey(s.Symbol.Key), "items[%d].footprint.symbols[%d]: invalid symbol key %q", i, j, s.Symbol.Key)
		c.require(validPath(s.Symbol.Path), "items[%d].footprint.symbols[%d]: invalid path %q", i, j, s.Symbol.Path)
		c.require(s.OldPath == "" || validPath(s.OldPath), "items[%d].footprint.symbols[%d]: invalid oldPath %q", i, j, s.OldPath)
	}
	for j, r := range fp.Refs {
		c.require(validPath(r.Path), "items[%d].footprint.refs[%d]: invalid path %q", i, j, r.Path)
	}
}

// validateOutbox checks every id in the outbox document of agent.
func validateOutbox(agent model.AgentID, ob *model.Outbox) error {
	var c checker
	c.require(ob.Agent == agent, "agent id %q does not match the ref", ob.Agent)
	for i, n := range ob.Needs {
		c.require(hex12Pattern.MatchString(n.ID), "needs[%d]: invalid id %q", i, n.ID)
		c.require(validKey(n.Key), "needs[%d]: invalid symbol key %q", i, n.Key)
		c.require(model.ValidItemID(n.Item), "needs[%d]: invalid item id %q", i, n.Item)
		c.require(validOptOID(n.PinnedStub), "needs[%d]: invalid pinnedStub %q", i, n.PinnedStub)
		c.require(validOptOID(n.PinnedCommit), "needs[%d]: invalid pinnedCommit %q", i, n.PinnedCommit)
	}
	for i := range ob.Messages {
		m := &ob.Messages[i]
		c.require(hex12Pattern.MatchString(m.ID), "messages[%d]: invalid id %q", i, m.ID)
		c.require(threadPattern.MatchString(m.Thread), "messages[%d]: invalid thread %q", i, m.Thread)
		c.require(m.Kind == model.ThreadSignature || m.Kind == model.ThreadNote, "messages[%d]: invalid kind %q", i, m.Kind)
		c.require(m.Kind != model.ThreadSignature || validKey(m.Subject), "messages[%d]: invalid symbol key %q", i, m.Subject)
		c.require(m.From == agent, "messages[%d]: from %q is not the outbox's agent", i, m.From)
		c.require(m.Item == "" || model.ValidItemID(m.Item), "messages[%d]: invalid item id %q", i, m.Item)
		for _, it := range m.Items {
			c.require(model.ValidItemID(it), "messages[%d]: invalid item id %q", i, it)
		}
		for _, id := range m.Seen {
			c.require(hex12Pattern.MatchString(id), "messages[%d]: invalid seen id %q", i, id)
		}
		if p := m.Proposal; p != nil {
			c.require(proposalPattern.MatchString(p.ID), "messages[%d]: invalid proposal id %q", i, p.ID)
			c.require(model.ValidAgentID(p.Owner), "messages[%d]: invalid proposal owner %q", i, p.Owner)
		}
	}
	for i, a := range ob.Accepts {
		c.require(threadPattern.MatchString(a.Thread), "accepts[%d]: invalid thread %q", i, a.Thread)
		c.require(proposalPattern.MatchString(a.Proposal), "accepts[%d]: invalid proposal id %q", i, a.Proposal)
	}
	return c.err
}

// validateClaim checks every id and path in the claim stored under key.
func validateClaim(key string, cl *model.Claim) error {
	var c checker
	c.require(cl.Key == key, "key %q does not match the ref", cl.Key)
	c.require(cl.Symbol.Key == "" || cl.Symbol.Key == key, "symbol key %q does not match the ref", cl.Symbol.Key)
	c.require(cl.Symbol.Path == "" || validPath(cl.Symbol.Path), "invalid symbol path %q", cl.Symbol.Path)
	c.require(model.ValidAgentID(cl.Owner), "invalid owner %q", cl.Owner)
	c.require(cl.Item == "" || model.ValidItemID(cl.Item), "invalid item id %q", cl.Item)
	c.require(cl.Epoch >= 0, "negative epoch")
	c.require(cl.LeaseSeconds >= 0, "negative leaseSeconds")
	if cl.Contract != nil {
		c.require(validOptOID(cl.Contract.StubCommit), "invalid stubCommit %q", cl.Contract.StubCommit)
	}
	for i, r := range cl.Requirements {
		c.require(model.ValidAgentID(r.From), "requirements[%d]: invalid agent %q", i, r.From)
	}
	return c.err
}

// decodeDoc unmarshals a document into v after checking its format, so a
// document from a newer gitlet is reported as such rather than as invalid.
func decodeDoc(data []byte, v any) error {
	var head struct {
		Format *int `json:"format"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	switch {
	case head.Format == nil:
		return errors.New("no format")
	case *head.Format > model.Format:
		return fmt.Errorf("%w: format %d, this gitlet reads %d", errNewerFormat, *head.Format, model.Format)
	case *head.Format < 1:
		return fmt.Errorf("invalid format %d", *head.Format)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	return nil
}

func decodeState(agent model.AgentID, data []byte) (model.AgentState, error) {
	var st model.AgentState
	if err := decodeDoc(data, &st); err != nil {
		return model.AgentState{}, err
	}
	if err := validateState(agent, &st); err != nil {
		return model.AgentState{}, err
	}
	return st, nil
}

func decodeOutbox(agent model.AgentID, data []byte) (model.Outbox, error) {
	var ob model.Outbox
	if err := decodeDoc(data, &ob); err != nil {
		return model.Outbox{}, err
	}
	if err := validateOutbox(agent, &ob); err != nil {
		return model.Outbox{}, err
	}
	return ob, nil
}

func decodeClaim(key string, data []byte) (model.Claim, error) {
	var cl model.Claim
	if err := decodeDoc(data, &cl); err != nil {
		return model.Claim{}, err
	}
	if err := validateClaim(key, &cl); err != nil {
		return model.Claim{}, err
	}
	return cl, nil
}

// encodeDoc marshals a document and enforces MaxWriteDoc.
func encodeDoc(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxWriteDoc {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(data), MaxWriteDoc)
	}
	return data, nil
}

// encodeState prepares this agent's state for publishing: it sets the
// format, drops items settled more than an hour ago (docs/SPEC.md, section
// 6.4) and, when the document is still too large, every settled item. It
// refuses with ErrTooLarge when that is not enough.
func encodeState(me model.AgentID, st model.AgentState, now time.Time) (model.AgentState, []byte, error) {
	st.Format = model.Format
	if st.Agent.ID == "" {
		st.Agent.ID = me
	}
	if err := validateState(me, &st); err != nil {
		return st, nil, fmt.Errorf("invalid state: %w", err)
	}
	st.Items = dropSettled(st.Items, now.Add(-settledAge))
	data, err := encodeDoc(st)
	if errors.Is(err, ErrTooLarge) {
		st.Items = dropSettled(st.Items, time.Time{})
		data, err = encodeDoc(st)
	}
	return st, data, err
}

// dropSettled returns items without the landed and abandoned ones closed
// before cutoff, or without every one of them when cutoff is zero. The input
// slice is not modified.
func dropSettled(items []model.WorkItem, cutoff time.Time) []model.WorkItem {
	out := make([]model.WorkItem, 0, len(items))
	for _, it := range items {
		if !it.Status.Open() {
			closed := it.UpdatedAt
			if it.ClosedAt != nil {
				closed = *it.ClosedAt
			}
			if cutoff.IsZero() || closed.Before(cutoff) {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

func encodeOutbox(me model.AgentID, ob model.Outbox) (model.Outbox, []byte, error) {
	ob.Format = model.Format
	if ob.Agent == "" {
		ob.Agent = me
	}
	if err := validateOutbox(me, &ob); err != nil {
		return ob, nil, fmt.Errorf("invalid outbox: %w", err)
	}
	data, err := encodeDoc(ob)
	return ob, data, err
}

func encodeClaim(c model.Claim) (model.Claim, []byte, error) {
	c.Format = model.Format
	if !validKey(c.Key) {
		return c, nil, fmt.Errorf("invalid symbol key %q", c.Key)
	}
	if err := validateClaim(c.Key, &c); err != nil {
		return c, nil, fmt.Errorf("invalid claim: %w", err)
	}
	data, err := encodeDoc(c)
	return c, data, err
}

// commitDoc writes a metadata commit: a tree holding one file, a fixed
// message, and parent as its only parent when set.
func (b *Bus) commitDoc(ctx context.Context, file string, data []byte, message, parent string) (string, error) {
	blob, err := b.repo.HashBlob(ctx, data)
	if err != nil {
		return "", err
	}
	tree, err := b.repo.MakeTree(ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", OID: blob, Name: file}})
	if err != nil {
		return "", err
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	return b.repo.CommitTree(ctx, tree, parents, message, gitx.Identity{})
}

// readDoc reads one document file of a commit, refusing documents over
// MaxReadDoc.
func (b *Bus) readDoc(ctx context.Context, oid, file string) ([]byte, error) {
	data, err := b.repo.ReadFile(ctx, oid, file)
	if errors.Is(err, gitx.ErrNotFound) {
		return nil, fmt.Errorf("commit %s has no %s: %w", oid, file, err)
	}
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReadDoc {
		return nil, fmt.Errorf("%s of %s is too large (%d bytes, limit %d)", file, oid, len(data), MaxReadDoc)
	}
	return data, nil
}

// objectReader is the part of *gitx.Reader that Load uses.
type objectReader interface {
	ReadObject(spec string, max int64) (data []byte, size int64, ok bool, err error)
	Close() error
}

// objectReaderOpener lets a Repo supply its own objectReader. A *gitx.Reader
// can only be made by gitx, so the tests' fake repository implements this.
type objectReaderOpener interface {
	openObjectReader(ctx context.Context) (objectReader, error)
}

// openReader starts one batch reader for a series of document reads.
func (b *Bus) openReader(ctx context.Context) (objectReader, error) {
	if o, ok := b.repo.(objectReaderOpener); ok {
		return o.openObjectReader(ctx)
	}
	rd, err := b.repo.OpenReader(ctx)
	if err != nil {
		return nil, err
	}
	return rd, nil
}
