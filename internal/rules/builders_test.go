package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/model"
)

const (
	me    = model.AgentID("claude-code-laptop-aaaaaa")
	bob   = model.AgentID("codex-ci-bbbbbb")
	carol = model.AgentID("cursor-desk-cccccc")

	myItem    = model.ItemID("a00000000001")
	myItem2   = model.ItemID("a00000000002")
	bobItem   = model.ItemID("b00000000001")
	carolItem = model.ItemID("c00000000001")
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func hashOf(s string) string { return model.ContentID(s) }

// fn builds an exported symbol; a qualified name "Type.m" makes a method.
func fn(lang, path, qualified, sig string) model.Symbol {
	container, name := splitQualified(qualified)
	kind := "function"
	if container != "" {
		kind = "method"
	}
	return model.Symbol{
		Key: model.SymbolKey(lang, path, qualified), Lang: lang, Path: path,
		Name: name, Qualified: qualified, Container: container, Kind: kind,
		Signature: sig, SigHash: hashOf(sig), BodyHash: hashOf("body of " + qualified),
		Exported: true,
	}
}

func sigChange(s model.Symbol, newSig string, breaking bool) model.SymbolChange {
	n := s
	n.Signature, n.SigHash = newSig, hashOf(newSig)
	return model.SymbolChange{Change: model.ChangeSignature, Breaking: breaking, SigChanged: true,
		OldSignature: s.Signature, OldSigHash: s.SigHash, Symbol: n}
}

func bodyChange(s model.Symbol) model.SymbolChange {
	n := s
	n.BodyHash = hashOf("edited " + s.BodyHash)
	return model.SymbolChange{Change: model.ChangeBody, BodyChanged: true, Symbol: n}
}

func removedChange(s model.Symbol) model.SymbolChange {
	return model.SymbolChange{Change: model.ChangeRemoved, Breaking: true, Symbol: s}
}

func addedChange(s model.Symbol) model.SymbolChange {
	return model.SymbolChange{Change: model.ChangeAdded, Symbol: s}
}

func renamedChange(s model.Symbol, qualified string) model.SymbolChange {
	n := fn(s.Lang, s.Path, qualified, strings.Replace(s.Signature, s.Name, qualified, 1))
	return model.SymbolChange{Change: model.ChangeRenamed, Breaking: true, OldName: s.Qualified,
		OldSignature: s.Signature, OldSigHash: s.SigHash, Symbol: n}
}

func movedChange(s model.Symbol, path string, breaking bool) model.SymbolChange {
	n := fn(s.Lang, path, s.Qualified, s.Signature)
	return model.SymbolChange{Change: model.ChangeMoved, Breaking: breaking, OldPath: s.Path, Symbol: n}
}

// ref builds a resolved reference to module.
func ref(name, lang, path string, line int, module string) model.Ref {
	return model.Ref{Name: name, Kind: model.RefCall, Lang: lang, Path: path, Line: line, Module: module, Resolved: true}
}

// methodRef builds an unresolved reference through a value qualifier.
func methodRef(name, qualifier, lang, path string, line int) model.Ref {
	return model.Ref{Name: name, Qualifier: qualifier, Kind: model.RefCall, Lang: lang, Path: path, Line: line}
}

type itemOpt func(*model.WorkItem)

func item(id model.ItemID, intent string, opts ...itemOpt) model.WorkItem {
	it := model.WorkItem{ID: id, Intent: intent, Branch: "gitlet/" + model.Slug(intent, 20) + "-" + string(id[:4]),
		Base: "base0", Head: "head-" + string(id), Status: model.ItemActive, OpenedAt: t0.Add(-time.Hour)}
	for _, o := range opts {
		o(&it)
	}
	return it
}

// changes adds symbol changes and their files (parsed, modified).
func changes(chs ...model.SymbolChange) itemOpt {
	return func(it *model.WorkItem) {
		for _, ch := range chs {
			it.Footprint.Symbols = append(it.Footprint.Symbols, ch)
			addFile(it, model.FileChange{Path: ch.Symbol.Path, Status: "M", Lang: ch.Symbol.Lang, Parsed: true})
		}
	}
}

func refs(rs ...model.Ref) itemOpt {
	return func(it *model.WorkItem) {
		for _, r := range rs {
			it.Footprint.Refs = append(it.Footprint.Refs, r)
			addFile(it, model.FileChange{Path: r.Path, Status: "M", Lang: r.Lang, Parsed: true})
		}
	}
}

func files(fs ...model.FileChange) itemOpt {
	return func(it *model.WorkItem) {
		for _, f := range fs {
			addFile(it, f)
		}
	}
}

func addFile(it *model.WorkItem, f model.FileChange) {
	for _, x := range it.Footprint.Files {
		if x.Path == f.Path {
			return
		}
	}
	it.Footprint.Files = append(it.Footprint.Files, f)
}

func unparsed(path string) model.FileChange { return model.FileChange{Path: path, Status: "M"} }
func added(path string) model.FileChange    { return model.FileChange{Path: path, Status: "A"} }

func submitted() itemOpt   { return func(it *model.WorkItem) { it.Status = model.ItemSubmitted } }
func provisional() itemOpt { return func(it *model.WorkItem) { it.Provisional = true } }
func broadFP() itemOpt     { return func(it *model.WorkItem) { it.Footprint.Broad = true } }
func status(s model.ItemStatus) itemOpt {
	return func(it *model.WorkItem) { it.Status = s }
}

// vb builds views.
type vb struct{ v View }

func view() *vb {
	return &vb{View{
		Me:        me,
		Mine:      model.AgentState{Format: 1, Agent: model.Agent{ID: me}, LeaseSeconds: 600},
		MyOutbox:  model.Outbox{Format: 1, Agent: me},
		Peers:     map[model.AgentID]Peer{},
		Claims:    map[string]ClaimView{},
		Main:      map[model.ItemID]MainView{},
		ClashSeen: map[string]time.Time{},
		Limits:    Limits{AutoClaim: true},
		Now:       t0,
	}}
}

func (b *vb) mine(items ...model.WorkItem) *vb {
	b.v.Mine.Items = append(b.v.Mine.Items, items...)
	return b
}

func (b *vb) peer(id model.AgentID, items ...model.WorkItem) *vb {
	p := b.v.Peers[id]
	p.State.Format, p.State.Agent.ID, p.State.LeaseSeconds = 1, id, 600
	p.Outbox.Agent = id
	p.State.Items = append(p.State.Items, items...)
	b.v.Peers[id] = p
	return b
}

func (b *vb) lapsed(id model.AgentID, idle time.Time) *vb {
	p := b.v.Peers[id]
	p.Lapsed, p.IdleSince = true, &idle
	b.v.Peers[id] = p
	return b
}

func (b *vb) claim(key string, owner model.AgentID, it model.ItemID, epoch int, opts ...func(*ClaimView)) *vb {
	cv := ClaimView{Claim: model.Claim{Format: 1, Key: key, Owner: owner, Item: it, Epoch: epoch, LeaseSeconds: 600}, OID: "oid-" + key[:6]}
	for _, o := range opts {
		o(&cv)
	}
	b.v.Claims[key] = cv
	return b
}

func withSymbol(s model.Symbol) func(*ClaimView) { return func(cv *ClaimView) { cv.Claim.Symbol = s } }
func withContract(sig string) func(*ClaimView) {
	return func(cv *ClaimView) {
		cv.Claim.Contract = &model.Contract{Signature: sig, SigHash: hashOf(sig), StubCommit: "stub1", PublishedAt: t0}
	}
}
func autoClaim() func(*ClaimView) { return func(cv *ClaimView) { cv.Claim.Auto = true } }
func landed() func(*ClaimView)    { return func(cv *ClaimView) { cv.Landed = true } }
func claimLapsed(idle time.Time) func(*ClaimView) {
	return func(cv *ClaimView) { cv.Lapsed, cv.IdleSince = true, &idle }
}

func (b *vb) need(agent model.AgentID, n model.Need) *vb {
	if n.ID == "" {
		n.ID = model.ContentID("need", string(agent), n.Key, string(n.Item))
	}
	if agent == me {
		b.v.MyOutbox.Needs = append(b.v.MyOutbox.Needs, n)
		return b
	}
	p := b.v.Peers[agent]
	p.Outbox.Needs = append(p.Outbox.Needs, n)
	b.v.Peers[agent] = p
	return b
}

func (b *vb) msg(m model.Message) *vb {
	if m.Kind == "" {
		m.Kind = model.ThreadSignature
	}
	if m.From == me {
		b.v.MyOutbox.Messages = append(b.v.MyOutbox.Messages, m)
		return b
	}
	p := b.v.Peers[m.From]
	p.Outbox.Messages = append(p.Outbox.Messages, m)
	b.v.Peers[m.From] = p
	return b
}

func (b *vb) accept(from model.AgentID, thread, proposal string) *vb {
	a := model.Accept{Thread: thread, Proposal: proposal, At: t0}
	if from == me {
		b.v.MyOutbox.Accepts = append(b.v.MyOutbox.Accepts, a)
		return b
	}
	p := b.v.Peers[from]
	p.Outbox.Accepts = append(p.Outbox.Accepts, a)
	b.v.Peers[from] = p
	return b
}

func (b *vb) conflict(mine, other model.ItemID, paths ...string) *vb {
	b.v.Conflicts = append(b.v.Conflicts, Conflict{Mine: mine, Other: other, Paths: paths})
	return b
}

func (b *vb) main(it model.ItemID, fp model.Footprint) *vb {
	b.v.Main[it] = MainView{Main: "main1", Footprint: fp}
	return b
}

func (b *vb) with(f func(*View)) *vb { f(&b.v); return b }

// as returns the same world seen by another agent: its state becomes Mine
// and mine becomes a peer.
func (b *vb) as(agent model.AgentID) View {
	v := b.v
	v.Peers = map[model.AgentID]Peer{}
	for id, p := range b.v.Peers {
		if id != agent {
			v.Peers[id] = p
		}
	}
	v.Peers[b.v.Me] = Peer{State: b.v.Mine, Outbox: b.v.MyOutbox}
	v.Me = agent
	v.Mine = b.v.Peers[agent].State
	v.MyOutbox = b.v.Peers[agent].Outbox
	return v
}

func ofKind(ns []model.Note, kind model.NoteKind) []model.Note {
	var out []model.Note
	for _, n := range ns {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// one returns the only note of a kind, failing otherwise.
func one(t *testing.T, ns []model.Note, kind model.NoteKind) model.Note {
	t.Helper()
	got := ofKind(ns, kind)
	if len(got) != 1 {
		t.Fatalf("want one %s note, got %d: %s", kind, len(got), summary(ns))
	}
	return got[0]
}

func none(t *testing.T, ns []model.Note, kind model.NoteKind) {
	t.Helper()
	if got := ofKind(ns, kind); len(got) > 0 {
		t.Fatalf("want no %s note, got %s", kind, summary(got))
	}
}

func summary(ns []model.Note) string {
	var ss []string
	for _, n := range ns {
		ss = append(ss, "["+string(n.Severity)+"] "+string(n.Kind)+": "+n.Title)
	}
	return "\n  " + strings.Join(ss, "\n  ")
}

func tools(n model.Note) []string {
	var out []string
	for _, o := range n.Options {
		if o.Tool != "" {
			out = append(out, o.Tool)
		} else {
			out = append(out, "$ "+o.Command)
		}
	}
	return out
}

func optionFor(t *testing.T, n model.Note, tool string) model.Option {
	t.Helper()
	for _, o := range n.Options {
		if o.Tool == tool {
			return o
		}
	}
	t.Fatalf("note %s has no %s option: %v", n.Kind, tool, tools(n))
	return model.Option{}
}

func wantArgs(t *testing.T, o model.Option, want map[string]any) {
	t.Helper()
	if len(o.Args) != len(want) {
		t.Fatalf("%s args = %v, want %v", o.Tool, o.Args, want)
	}
	for k, v := range want {
		if o.Args[k] != v {
			t.Fatalf("%s args = %v, want %v", o.Tool, o.Args, want)
		}
	}
}

// freeText are arguments the agent writes itself; options leave them out.
var freeText = []string{"message", "intent", "reason", "requirement", "title", "body"}

// checkNotes verifies what every note must satisfy: sorted output, ids from
// NoteID, a title, Thread set when discuss is offered, and options that name
// a verb and its arguments exactly as the verb table does.
func checkNotes(t *testing.T, ns []model.Note) {
	t.Helper()
	for i, n := range ns {
		if i > 0 {
			p := ns[i-1]
			if p.Severity.Rank() > n.Severity.Rank() ||
				(p.Severity == n.Severity && (p.Kind > n.Kind || (p.Kind == n.Kind && p.ID >= n.ID))) {
				t.Fatalf("notes not sorted at %d: %s", i, summary(ns))
			}
		}
		if n.ID != model.NoteID(n.Kind, n.Subject, n.Item) {
			t.Errorf("%s: id %s is not NoteID(kind, subject, item)", n.Kind, n.ID)
		}
		if n.Title == "" || n.Version == "" || n.Severity == "" {
			t.Errorf("%s: missing title, version or severity: %+v", n.Kind, n)
		}
		for _, o := range n.Options {
			if o.Label == "" {
				t.Errorf("%s: option without label: %+v", n.Kind, o)
			}
			if o.Tool == "" {
				if o.Command == "" {
					t.Errorf("%s: option with neither tool nor command", n.Kind)
				}
				continue
			}
			verb, ok := api.Lookup(o.Tool)
			if !ok {
				t.Errorf("%s: option names unknown verb %q", n.Kind, o.Tool)
				continue
			}
			for k, v := range o.Args {
				if !slices.ContainsFunc(verb.Args, func(a api.Arg) bool { return a.Name == k }) {
					t.Errorf("%s: %s has no argument %q", n.Kind, o.Tool, k)
				}
				if slices.Contains(freeText, k) {
					t.Errorf("%s: option fills free-text argument %q", n.Kind, k)
				}
				if s, ok := v.(string); !ok || s == "" {
					t.Errorf("%s: %s argument %q is %v", n.Kind, o.Tool, k, v)
				}
			}
			if o.Tool == "discuss" && n.Thread == "" {
				t.Errorf("%s offers discuss without a thread", n.Kind)
			}
			if note, ok := o.Args["note"]; ok && note != n.ID {
				t.Errorf("%s: option %s names note %v, want %s", n.Kind, o.Tool, note, n.ID)
			}
		}
	}
}

// eval runs Evaluate and checks the invariants.
func eval(t *testing.T, v View) []model.Note {
	t.Helper()
	ns := Evaluate(v)
	checkNotes(t, ns)
	return ns
}
