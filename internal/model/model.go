// Package model holds the types shared by every gitlet package: agents, work
// items, footprints, claims, outbox entries, notes and threads. The JSON
// shapes here are the wire format stored in refs under refs/gitlet/, so field
// names and tags are part of the protocol (see docs/SPEC.md, section 6).
package model

import "time"

// Format is the version of every JSON document gitlet writes to refs.
// Readers skip documents with a higher format and report them.
const Format = 1

// AgentID identifies one agent session, for example "claude-code-laptop-3fa9c1".
type AgentID string

// ItemID identifies one work item (12 hex characters).
type ItemID string

// Agent describes who is behind an AgentID.
type Agent struct {
	ID     AgentID `json:"id"`
	Client string  `json:"client"` // "claude-code", "codex", "cursor", "cli", ...
	Host   string  `json:"host"`
	User   string  `json:"user,omitempty"`
}

// ItemStatus is the lifecycle state of a work item (docs/SPEC.md, section 7).
type ItemStatus string

const (
	ItemActive    ItemStatus = "active"
	ItemSubmitted ItemStatus = "submitted"
	ItemLanded    ItemStatus = "landed"
	ItemAbandoned ItemStatus = "abandoned"
)

// Open reports whether an item in this status still takes part in the rules.
func (s ItemStatus) Open() bool { return s == ItemActive || s == ItemSubmitted }

// WorkItem is one task on one branch in one worktree.
type WorkItem struct {
	ID          ItemID     `json:"id"`
	Intent      string     `json:"intent"`
	Provisional bool       `json:"provisional,omitempty"` // opened implicitly; intent taken from the branch name
	Branch      string     `json:"branch"`
	Base        string     `json:"base"`               // merge base with the remote main branch
	Snapshot    string     `json:"snapshot,omitempty"` // last snapshot commit (active items)
	Head        string     `json:"head,omitempty"`     // branch head at the last footprint
	Worktree    string     `json:"worktree,omitempty"` // informational, local path
	Status      ItemStatus `json:"status"`
	PR          string     `json:"pr,omitempty"`            // pull request URL once submitted
	SnapshotErr string     `json:"snapshotError,omitempty"` // why the last wip push was rejected
	OpenedAt    time.Time  `json:"openedAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClosedAt    *time.Time `json:"closedAt,omitempty"` // when it landed or was abandoned
	Footprint   Footprint  `json:"footprint"`
}

// Param is one parameter of a function or method, as callers see it.
type Param struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`        // normalized type text, "" when untyped
	Optional    bool   `json:"optional,omitempty"`    // optional or has a default
	Variadic    bool   `json:"variadic,omitempty"`    // ...T, *args, **kwargs, rest
	KeywordOnly bool   `json:"keywordOnly,omitempty"` // Python parameters after * or *args
}

// Member is one exported field, member, variant or interface method of a
// type-like symbol.
type Member struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"` // normalized type or method signature
	Optional bool   `json:"optional,omitempty"`
	Method   bool   `json:"method,omitempty"`
}

// Symbol is one definition found in a source file.
type Symbol struct {
	Key       string   `json:"key"` // SymbolKey(lang, path, qualified)
	Lang      string   `json:"lang"`
	Path      string   `json:"path"`
	Name      string   `json:"name"`                // short name, e.g. parseConfig
	Qualified string   `json:"qualified"`           // Container.member or name, joined with "."
	Container string   `json:"container,omitempty"` // receiver, class, impl or trait type
	Kind      string   `json:"kind"`                // function, method, class, struct, interface, type, enum, trait, const, var
	Signature string   `json:"signature"`           // normalized structural declaration (docs/SPEC.md, section 9.2)
	SigHash   string   `json:"sigHash"`
	BodyHash  string   `json:"bodyHash"`
	Params    []Param  `json:"params,omitempty"`  // functions and methods
	Results   string   `json:"results,omitempty"` // normalized return type
	Members   []Member `json:"members,omitempty"` // type-like symbols
	StartLine int      `json:"startLine"`         // 1-based, inclusive
	EndLine   int      `json:"endLine"`           // 1-based, inclusive
	StartByte int      `json:"startByte"`         // byte range of the whole definition
	EndByte   int      `json:"endByte"`
	Exported  bool     `json:"exported"`

	// Head is the signature without the parts Params, Results and Members
	// describe: kind words and method markers, async, the receiver, type
	// parameters, bases and where clauses. Classifying a change compares it,
	// so nothing outside the structured parts changes unseen (docs/SPEC.md,
	// section 9.4). Extractors fill it; it is not stored, and an empty Head
	// stands for the whole signature.
	Head string `json:"-"`
}

// Ref returns the canonical human form "path:Qualified" used in tool arguments.
func (s Symbol) Ref() string { return s.Path + ":" + s.Qualified }

// TypeLike reports whether the symbol is a struct, class, interface, type
// alias, enum or trait.
func (s Symbol) TypeLike() bool {
	switch s.Kind {
	case "struct", "class", "interface", "type", "enum", "trait":
		return true
	}
	return false
}

// ChangeKind says how a symbol changed between the base and the head.
type ChangeKind string

const (
	ChangeAdded     ChangeKind = "added"
	ChangeRemoved   ChangeKind = "removed"
	ChangeSignature ChangeKind = "signature"
	ChangeBody      ChangeKind = "body"
	ChangeMoved     ChangeKind = "moved"   // file renamed, same qualified name
	ChangeRenamed   ChangeKind = "renamed" // same body, new name
)

// SymbolChange is one changed symbol in a footprint. For removed symbols,
// Symbol is the old version.
type SymbolChange struct {
	Change       ChangeKind `json:"change"`
	Breaking     bool       `json:"breaking,omitempty"` // docs/SPEC.md, section 9.3
	SigChanged   bool       `json:"sigChanged,omitempty"`
	BodyChanged  bool       `json:"bodyChanged,omitempty"`
	OldSignature string     `json:"oldSignature,omitempty"`
	OldSigHash   string     `json:"oldSigHash,omitempty"`
	OldPath      string     `json:"oldPath,omitempty"` // moved
	OldLang      string     `json:"oldLang,omitempty"` // moved to a file of another language (.js to .ts)
	OldName      string     `json:"oldName,omitempty"` // renamed (old qualified name)
	Symbol       Symbol     `json:"symbol"`
}

// SignatureChanging reports whether the change alters what callers see:
// a signature change, removal, rename, or a move that changes the module.
func (c SymbolChange) SignatureChanging() bool {
	switch c.Change {
	case ChangeSignature, ChangeRemoved, ChangeRenamed:
		return true
	case ChangeMoved:
		return c.SigChanged || c.Breaking
	}
	return false
}

// FileChange is one changed file in a footprint.
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"` // set for renames
	Status  string `json:"status"`            // A, M, D, R, C, T
	Lang    string `json:"lang,omitempty"`
	Parsed  bool   `json:"parsed"`
}

// RefKind says how code refers to a symbol.
type RefKind string

const (
	RefCall      RefKind = "call"
	RefType      RefKind = "type"
	RefConstruct RefKind = "construct"
	RefExtends   RefKind = "extends"
)

// Ref is one reference made from added or changed code.
type Ref struct {
	Name      string  `json:"name"`
	Qualifier string  `json:"qualifier,omitempty"` // receiver, package or namespace text, when present
	Kind      RefKind `json:"kind"`
	Lang      string  `json:"lang"`
	Path      string  `json:"path"`
	Line      int     `json:"line"`
	Module    string  `json:"module,omitempty"` // module the ref resolves to (docs/SPEC.md, section 9.5)
	Resolved  bool    `json:"resolved"`

	// Container is what a resolved ref's qualifier names inside Module, in
	// the form of Symbol.Container: a type ("Config" for Config.load,
	// Config::new or a Go method expression) or a Rust inline module. It is
	// "" when the qualifier names the module itself, or there is none.
	Container string `json:"container,omitempty"`

	// Receiver is set by an extractor when the qualifier is the value a
	// method is called on and the language tells it apart from a path, as
	// Rust does for x.f() and x::f(). The footprint package leaves such a
	// ref unresolved and clears the flag, so it is never stored.
	Receiver bool `json:"-"`
}

// Footprint is what a work item changes, at file and symbol level.
type Footprint struct {
	Base       string         `json:"base"`
	Head       string         `json:"head"`
	Version    int            `json:"version,omitempty"` // the footprint.Version that computed it; 0 before versions were recorded
	Broad      bool           `json:"broad,omitempty"`
	Files      []FileChange   `json:"files,omitempty"`
	Symbols    []SymbolChange `json:"symbols,omitempty"`
	Refs       []Ref          `json:"refs,omitempty"`
	ComputedAt time.Time      `json:"computedAt"`
}

// AgentState is the content of refs/gitlet/agents/<agent>.
type AgentState struct {
	Format       int        `json:"format"`
	Agent        Agent      `json:"agent"`
	Heartbeat    time.Time  `json:"heartbeat"`
	LeaseSeconds int        `json:"leaseSeconds"`
	Items        []WorkItem `json:"items"`
}

// Need says an item depends on an interface someone else is changing.
type Need struct {
	ID            string    `json:"id"`
	Key           string    `json:"key"`  // symbol key
	Name          string    `json:"name"` // canonical "path:Qualified"
	Item          ItemID    `json:"item"`
	Requirement   string    `json:"requirement,omitempty"`
	PinnedSigHash string    `json:"pinnedSigHash,omitempty"` // set by use
	PinnedStub    string    `json:"pinnedStub,omitempty"`    // the contract's stub commit
	PinnedCommit  string    `json:"pinnedCommit,omitempty"`  // the local cherry-picked commit
	CreatedAt     time.Time `json:"createdAt"`
}

// Proposal is a concrete way to settle a contested interface or task.
type Proposal struct {
	ID        string  `json:"id"`                  // "p-" + 12 hex
	Owner     AgentID `json:"owner"`               // who will own the interface or task
	Signature string  `json:"signature,omitempty"` // the agreed signature, when known
	Summary   string  `json:"summary"`
}

// ThreadKind distinguishes bounded signature threads from plain conversations.
type ThreadKind string

const (
	ThreadSignature ThreadKind = "sig"
	ThreadNote      ThreadKind = "note"
)

// Message is one entry in a thread.
type Message struct {
	ID          string     `json:"id"`
	Thread      string     `json:"thread"`
	Kind        ThreadKind `json:"kind"`
	Subject     string     `json:"subject,omitempty"` // symbol key or path
	SubjectName string     `json:"subjectName,omitempty"`
	NoteKind    NoteKind   `json:"noteKind,omitempty"` // for note threads
	Items       []ItemID   `json:"items,omitempty"`    // participants' items when sent
	From        AgentID    `json:"from"`
	Item        ItemID     `json:"item,omitempty"`
	Text        string     `json:"text"`
	Proposal    *Proposal  `json:"proposal,omitempty"`
	Seen        []string   `json:"seen,omitempty"` // ids of messages the author had seen
	At          time.Time  `json:"at"`
}

// Accept records that an agent accepted a proposal.
type Accept struct {
	Thread   string    `json:"thread"`
	Proposal string    `json:"proposal"`
	At       time.Time `json:"at"`
}

// Outbox is the content of refs/gitlet/outbox/<agent>: what an agent tells others.
type Outbox struct {
	Format   int       `json:"format"`
	Agent    AgentID   `json:"agent"`
	Needs    []Need    `json:"needs,omitempty"`
	Messages []Message `json:"messages,omitempty"`
	Accepts  []Accept  `json:"accepts,omitempty"`
}

// Contract is the published shape of an interface.
type Contract struct {
	Signature   string    `json:"signature"`
	SigHash     string    `json:"sigHash"`
	StubCommit  string    `json:"stubCommit"`
	Breaking    bool      `json:"breaking,omitempty"`
	PR          string    `json:"pr,omitempty"` // interface-only pull request
	PublishedAt time.Time `json:"publishedAt"`
}

// Requirement is a need another agent stated, recorded on a claim.
type Requirement struct {
	From AgentID `json:"from"`
	Text string  `json:"text"`
}

// Claim is the content of refs/gitlet/claims/<key>.
type Claim struct {
	Format       int           `json:"format"`
	Key          string        `json:"key"`
	Symbol       Symbol        `json:"symbol"`
	Owner        AgentID       `json:"owner"`
	Item         ItemID        `json:"item"`
	LeaseSeconds int           `json:"leaseSeconds"`
	Epoch        int           `json:"epoch"`          // 0 at creation, +1 on every ownership transfer
	Auto         bool          `json:"auto,omitempty"` // taken automatically (gitlet.autoClaim), not by the agent
	Contract     *Contract     `json:"contract,omitempty"`
	Requirements []Requirement `json:"requirements,omitempty"`
	UpdatedAt    time.Time     `json:"updatedAt"`
}

// NoteKind names the rule that produced a note (docs/SPEC.md, section 10).
type NoteKind string

const (
	NoteCalleeChanged     NoteKind = "callee_changed"
	NoteCallersAffected   NoteKind = "callers_affected"
	NoteSignatureClash    NoteKind = "signature_clash"
	NoteBodyOverlap       NoteKind = "body_overlap"
	NotePredictedConflict NoteKind = "predicted_conflict"
	NoteFileOverlap       NoteKind = "file_overlap"
	NoteHotFile           NoteKind = "hot_file"
	NoteMigration         NoteKind = "migration"
	NoteNeeded            NoteKind = "needed"
	NoteReady             NoteKind = "ready"
	NoteDrift             NoteKind = "drift"
	NoteDiverged          NoteKind = "diverged"
	NoteStillWaiting      NoteKind = "still_waiting"
	NoteLapsed            NoteKind = "lapsed"
	NoteDuplicate         NoteKind = "duplicate"
	NoteOutsideBranch     NoteKind = "outside_branch"
	NoteStale             NoteKind = "stale"
	NoteNewCallers        NoteKind = "new_callers"
	NoteBroad             NoteKind = "broad"
	NoteMessage           NoteKind = "message"
	NoteAgreement         NoteKind = "agreement"
	NoteUntitled          NoteKind = "untitled"
	NoteSameWorktree      NoteKind = "same_worktree"
)

// Severity says how urgently a note asks for action.
type Severity string

const (
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
	SeverityAct  Severity = "act"
)

// Rank orders severities: act 0, warn 1, info 2.
func (s Severity) Rank() int {
	switch s {
	case SeverityAct:
		return 0
	case SeverityWarn:
		return 1
	}
	return 2
}

// Lower returns the next lower severity (act -> warn -> info).
func (s Severity) Lower() Severity {
	switch s {
	case SeverityAct:
		return SeverityWarn
	}
	return SeverityInfo
}

// Option is one thing the agent can do about a note or a refusal: call Tool
// with Args, or run the shell Command.
type Option struct {
	Label   string         `json:"label"`
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Command string         `json:"command,omitempty"`
}

// Party is another agent and item a note is about.
type Party struct {
	Agent     AgentID    `json:"agent"`
	Item      ItemID     `json:"item,omitempty"`
	Intent    string     `json:"intent,omitempty"`
	Branch    string     `json:"branch,omitempty"`
	PR        string     `json:"pr,omitempty"`
	IdleSince *time.Time `json:"idleSince,omitempty"` // set when the agent has lapsed
}

// Quote is text another agent wrote. Renderers show it in a labelled block
// as data, never as instructions.
type Quote struct {
	From AgentID `json:"from"`
	Text string  `json:"text"`
}

// Note is a fact gitlet tells one agent, produced by a deterministic rule.
type Note struct {
	ID          string    `json:"id"`      // NoteID(kind, subject, item)
	Version     string    `json:"version"` // NoteVersion over the material facts
	Kind        NoteKind  `json:"kind"`
	Severity    Severity  `json:"severity"`
	Item        ItemID    `json:"item,omitempty"`    // the recipient's item
	Subject     string    `json:"subject,omitempty"` // symbol key, path, or other subject
	SubjectName string    `json:"subjectName,omitempty"`
	Thread      string    `json:"thread,omitempty"`
	Parties     []Party   `json:"parties,omitempty"`
	Title       string    `json:"title"`
	Body        string    `json:"body,omitempty"`
	Evidence    []string  `json:"evidence,omitempty"`
	Quotes      []Quote   `json:"quotes,omitempty"`
	Options     []Option  `json:"options,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Participant is one side of a thread.
type Participant struct {
	Agent AgentID `json:"agent"`
	Item  ItemID  `json:"item"`
}

// ThreadState is a thread assembled from its participants' outboxes.
type ThreadState struct {
	ID           string               `json:"id"`
	Kind         ThreadKind           `json:"kind"`
	Subject      string               `json:"subject"` // symbol key or path
	SubjectName  string               `json:"subjectName,omitempty"`
	NoteKind     NoteKind             `json:"noteKind,omitempty"`
	Participants []Participant        `json:"participants"`
	Messages     []Message            `json:"messages"`           // causal order
	Accepted     map[string][]AgentID `json:"accepted,omitempty"` // proposal id -> agents that accepted
	Agreement    *Proposal            `json:"agreement,omitempty"`
	Owner        AgentID              `json:"owner,omitempty"` // settled owner (agreement or fallback)
	FellBack     bool                 `json:"fellBack,omitempty"`
	OpenedAt     time.Time            `json:"openedAt"`
	Deadline     time.Time            `json:"deadline,omitempty"` // signature threads only
	Settled      bool                 `json:"settled,omitempty"`
}
