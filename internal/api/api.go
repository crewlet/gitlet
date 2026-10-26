// Package api defines gitlet's verbs once (docs/SPEC.md, section 4): their
// names, when to use them, their arguments, their MCP annotations, and the
// input and result types. The engine implements them; the MCP server and the
// CLI are generated from Verbs, and render turns results into text.
package api

import (
	"reflect"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// Outcome is the first word of every result.
type Outcome string

const (
	OK          Outcome = "ok"
	Refused     Outcome = "refused"
	NothingToDo Outcome = "nothing_to_do"
)

// Result is the envelope every verb returns (docs/SPEC.md, section 4.2).
type Result struct {
	Outcome      Outcome        `json:"outcome"`
	Summary      string         `json:"summary"`
	Details      []string       `json:"details,omitempty"`
	Options      []model.Option `json:"options,omitempty"` // refusals carry at least one
	Notes        []model.Note   `json:"notes,omitempty"`   // new notes (all open notes for status)
	MoreNotes    int            `json:"moreNotes,omitempty"`
	Resolved     []string       `json:"resolved,omitempty"`
	Open         int            `json:"open"`
	OpenAct      int            `json:"openAct"`
	StateAge     string         `json:"stateAge,omitempty"`
	OfflineSince *time.Time     `json:"offlineSince,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
	// AllNotes marks a status result: Notes lists every open note, uncapped,
	// under "open notes:" instead of "new notes:".
	AllNotes bool `json:"-"`
}

// Envelope returns the result itself; every reply type gets it by embedding.
func (r *Result) Envelope() *Result { return r }

// Output is what every verb returns.
type Output interface{ Envelope() *Result }

// Reply is a result with verb-specific data.
type Reply[D any] struct {
	Result
	Data D `json:"data"`
}

// Where picks the worktree a call acts on. Every input embeds it.
type Where struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"absolute path of the worktree to act on; default: the server's directory"`
}

// ---- inputs ----

type StartInput struct {
	Intent string   `json:"intent" jsonschema:"your task in one sentence, e.g. Add strict mode to config loading"`
	Branch string   `json:"branch,omitempty" jsonschema:"branch to create when on main; default gitlet/<slug>-<id>"`
	Paths  []string `json:"paths,omitempty" jsonschema:"files you plan to touch, checked for overlap now"`
	Where
}

type StatusInput struct{ Where }

type WhoInput struct {
	Target string `json:"target" jsonschema:"a path or a symbol such as src/config.ts:parseConfig"`
	Where
}

type WhyInput struct {
	Target string `json:"target" jsonschema:"a path, path:line, or a symbol"`
	Where
}

type ClaimInput struct {
	Symbol string `json:"symbol" jsonschema:"[path:]Qualified, e.g. src/config.ts:parseConfig or Config.load"`
	Note   string `json:"note,omitempty" jsonschema:"the note you are acting on, if any"`
	Where
}

type PublishInput struct {
	Symbol string `json:"symbol" jsonschema:"[path:]Qualified of a symbol you own"`
	Commit string `json:"commit,omitempty" jsonschema:"a stub commit to publish instead of the one gitlet builds"`
	Note   string `json:"note,omitempty"`
	Where
}

type NeedInput struct {
	Symbol      string `json:"symbol" jsonschema:"[path:]Qualified of the interface you depend on"`
	Requirement string `json:"requirement,omitempty" jsonschema:"what you need from it, e.g. must keep returning Config"`
	Note        string `json:"note,omitempty"`
	Where
}

type UseInput struct {
	Symbol string `json:"symbol" jsonschema:"[path:]Qualified of a published interface you need"`
	Note   string `json:"note,omitempty"`
	Where
}

type ReleaseInput struct {
	Symbol string `json:"symbol" jsonschema:"[path:]Qualified you claimed or need"`
	Note   string `json:"note,omitempty"`
	Where
}

// ProposalInput is a concrete way to settle a thread.
type ProposalInput struct {
	Owner     string `json:"owner" jsonschema:"me, them, or an agent id: who will own the interface or task"`
	Signature string `json:"signature,omitempty" jsonschema:"the agreed signature, when it is about an interface"`
	Summary   string `json:"summary" jsonschema:"one sentence describing the proposal"`
}

type DiscussInput struct {
	About    string         `json:"about" jsonschema:"a note id (n-...) or thread id (t-...); unique prefixes work"`
	Message  string         `json:"message" jsonschema:"what you want to say: ask what they need, or explain yours"`
	Proposal *ProposalInput `json:"proposal,omitempty"`
	Where
}

type AcceptInput struct {
	Proposal string `json:"proposal" jsonschema:"proposal id (p-...); unique prefixes work"`
	Where
}

type DismissInput struct {
	Note   string `json:"note" jsonschema:"note id (n-...); unique prefixes work"`
	As     string `json:"as" jsonschema:"not_needed (real but harmless) or false_alarm (gitlet is wrong)"`
	Reason string `json:"reason,omitempty"`
	Where
}

type UpdateInput struct {
	Note string `json:"note,omitempty"`
	Where
}

type SubmitInput struct {
	Title  string `json:"title,omitempty" jsonschema:"pull request title; default: your intent"`
	Body   string `json:"body,omitempty" jsonschema:"pull request body; gitlet appends its own lines"`
	Draft  bool   `json:"draft,omitempty"`
	Symbol string `json:"symbol,omitempty" jsonschema:"open an interface-only PR for this published symbol instead"`
	Where
}

type AbandonInput struct {
	Reason string `json:"reason,omitempty"`
	Item   string `json:"item,omitempty" jsonschema:"the task to stop, by the id status shows; default: your active task, else the submitted task of the current branch"`
	Where
}

// ---- data ----

// Similar is another item that looks like the same task.
type Similar struct {
	Agent  model.AgentID `json:"agent"`
	Item   model.ItemID  `json:"item"`
	Intent string        `json:"intent"`
	Branch string        `json:"branch,omitempty"`
	Score  float64       `json:"score"`
	Files  []string      `json:"files,omitempty"`
}

// CommitRef is a commit on main that may already do the task.
type CommitRef struct {
	OID     string `json:"oid"`
	Subject string `json:"subject"`
}

type StartData struct {
	Item        model.WorkItem `json:"item"`
	Branch      string         `json:"branch"`
	NewBranch   bool           `json:"newBranch,omitempty"`
	Adopted     bool           `json:"adopted,omitempty"` // a provisional item was adopted
	Similar     []Similar      `json:"similar,omitempty"`
	Touching    []WhoEntry     `json:"touching,omitempty"`
	AlreadyDone []CommitRef    `json:"alreadyDone,omitempty"`
}

// Overlap is another item touching the same code.
type Overlap struct {
	Agent   model.AgentID `json:"agent"`
	Item    model.ItemID  `json:"item"`
	Intent  string        `json:"intent"`
	Symbols []string      `json:"symbols,omitempty"`
	Files   []string      `json:"files,omitempty"`
}

// ItemView is a work item with what is going on around it.
type ItemView struct {
	Item        model.WorkItem `json:"item"`
	Overlaps    []Overlap      `json:"overlaps,omitempty"`
	Uncommitted bool           `json:"uncommitted,omitempty"`
	PRClosed    bool           `json:"prClosed,omitempty"` // submitted, and its pull request was closed without merging
}

// ClaimInfo is a claim as reported to users.
type ClaimInfo struct {
	Symbol       string              `json:"symbol"` // path:Qualified
	Owner        model.AgentID       `json:"owner"`
	Mine         bool                `json:"mine"`
	Auto         bool                `json:"auto,omitempty"`
	Lapsed       bool                `json:"lapsed,omitempty"`
	Epoch        int                 `json:"epoch"`
	Contract     *model.Contract     `json:"contract,omitempty"`
	Requirements []model.Requirement `json:"requirements,omitempty"`
	Waiting      []model.AgentID     `json:"waiting,omitempty"` // agents that need it
}

// NeedInfo is one of my needs with its owner and contract.
type NeedInfo struct {
	Need     model.Need      `json:"need"`
	Owner    model.AgentID   `json:"owner,omitempty"`
	Contract *model.Contract `json:"contract,omitempty"`
}

// PeerInfo summarizes another agent.
type PeerInfo struct {
	Agent     model.Agent `json:"agent"`
	Items     []string    `json:"items"` // intents
	Lapsed    bool        `json:"lapsed,omitempty"`
	IdleSince *time.Time  `json:"idleSince,omitempty"`
}

type StatusData struct {
	Agent    model.Agent         `json:"agent"`
	Worktree string              `json:"worktree"`
	Remote   string              `json:"remote,omitempty"`
	Main     string              `json:"main"`
	ReadOnly bool                `json:"readOnly,omitempty"`
	Items    []ItemView          `json:"items"`
	Claims   []ClaimInfo         `json:"claims,omitempty"`
	Needs    []NeedInfo          `json:"needs,omitempty"`
	Threads  []model.ThreadState `json:"threads,omitempty"`
	Others   []PeerInfo          `json:"others,omitempty"`
}

// WhoEntry is one agent touching a target.
type WhoEntry struct {
	Agent  model.AgentID `json:"agent"`
	Item   model.ItemID  `json:"item,omitempty"`
	Intent string        `json:"intent,omitempty"`
	Role   string        `json:"role"` // changes, references, owns, needs, branch
	Detail string        `json:"detail,omitempty"`
	Lapsed bool          `json:"lapsed,omitempty"`
	PR     string        `json:"pr,omitempty"`
}

type WhoData struct {
	Target  string     `json:"target"`
	Entries []WhoEntry `json:"entries"`
}

type WhyData struct {
	Target  string        `json:"target"`
	Commit  string        `json:"commit,omitempty"`
	Subject string        `json:"subject,omitempty"`
	Author  string        `json:"author,omitempty"`
	When    *time.Time    `json:"when,omitempty"`
	Item    model.ItemID  `json:"item,omitempty"`
	Agent   model.AgentID `json:"agent,omitempty"`
	Intent  string        `json:"intent,omitempty"`
	PR      string        `json:"pr,omitempty"`
	Claim   *ClaimInfo    `json:"claim,omitempty"`
}

type ClaimData struct {
	Claim ClaimInfo `json:"claim"`
}

type PublishData struct {
	Symbol            string          `json:"symbol"`
	Contract          model.Contract  `json:"contract"`
	Waiting           []model.AgentID `json:"waiting,omitempty"`
	ProposalSignature string          `json:"proposalSignature,omitempty"` // the agreed proposal's text, to compare
}

type NeedData struct {
	Need     model.Need      `json:"need"`
	Owner    model.AgentID   `json:"owner,omitempty"`
	Contract *model.Contract `json:"contract,omitempty"`
}

type UseData struct {
	Symbol   string `json:"symbol"`
	SigHash  string `json:"sigHash"`
	Stub     string `json:"stub"`
	Commit   string `json:"commit"`             // the picked commit on your branch
	Reverted string `json:"reverted,omitempty"` // the previously picked commit, reverted
}

type ReleaseData struct {
	Symbol   string   `json:"symbol"`
	Released []string `json:"released"` // "claim", "need"
}

type DiscussData struct {
	Thread  model.ThreadState `json:"thread"`
	Message model.Message     `json:"message"`
}

type AcceptData struct {
	Thread model.ThreadState `json:"thread"`
	Agreed bool              `json:"agreed"`
}

type DismissData struct {
	Note string `json:"note"`
	As   string `json:"as"`
}

type UpdateData struct {
	Main      string   `json:"main"`
	Head      string   `json:"head,omitempty"`
	Rebased   bool     `json:"rebased"`
	Dropped   []string `json:"dropped,omitempty"` // stub commits dropped because their contracts landed
	Conflicts []string `json:"conflicts,omitempty"`
}

// Check is one submit check.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type SubmitData struct {
	Branch     string   `json:"branch"`
	Pushed     bool     `json:"pushed"`
	PR         string   `json:"pr,omitempty"`
	CompareURL string   `json:"compareUrl,omitempty"`
	Checks     []Check  `json:"checks,omitempty"`
	Conflicts  []string `json:"conflicts,omitempty"`
}

type AbandonData struct {
	Item     model.ItemID `json:"item"`
	Released []string     `json:"released,omitempty"`
}

// ---- the verb table ----

// Arg describes one argument for the CLI and for documentation. MCP input
// schemas come from the Input type's fields and jsonschema tags.
type Arg struct {
	Name     string
	Required bool // required arguments are positional in the CLI, in order
	Kind     string
	Help     string
	Enum     []string
}

// Arg kinds.
const (
	KindString  = "string"
	KindStrings = "strings"
	KindBool    = "bool"
	KindObject  = "object"
)

// Verb is one entry of the verb table.
type Verb struct {
	Name        string
	Summary     string // when to use it; the MCP tool description and CLI short help
	Args        []Arg
	ReadOnly    bool
	Idempotent  bool
	Destructive bool
	OpenWorld   bool
	Input       reflect.Type // the input struct type
	Data        reflect.Type // the reply data type
}

// NewInput returns a pointer to a zero input for the verb.
func (v Verb) NewInput() any { return reflect.New(v.Input).Interface() }

// Lookup returns a verb by name.
func Lookup(name string) (Verb, bool) {
	for _, v := range Verbs {
		if v.Name == name {
			return v, true
		}
	}
	return Verb{}, false
}

func t[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

var noteArg = Arg{Name: "note", Kind: KindString, Help: "the note you are acting on"}

// Verbs is the verb table, in the order the CLI lists them.
var Verbs = []Verb{
	{
		Name:    "start",
		Summary: "Call before you edit code for a new task. States your task, puts you on a work branch, and shows anyone already doing similar or overlapping work.",
		Args: []Arg{
			{Name: "intent", Required: true, Kind: KindString, Help: "your task in one sentence"},
			{Name: "branch", Kind: KindString, Help: "branch to create when on main"},
			{Name: "paths", Kind: KindStrings, Help: "files you plan to touch"},
		},
		Input: t[StartInput](), Data: t[StartData](),
	},
	{
		Name:     "status",
		Summary:  "Call when you begin or resume, after your context was compacted, or whenever unsure. Shows your task, what overlaps it, claims, needs, threads, and every open note.",
		ReadOnly: true,
		Input:    t[StatusInput](), Data: t[StatusData](),
	},
	{
		Name:     "who",
		Summary:  "Before editing a file or symbol, check who else is changing it, references it, owns it or needs it right now.",
		Args:     []Arg{{Name: "target", Required: true, Kind: KindString, Help: "a path or a symbol"}},
		ReadOnly: true,
		Input:    t[WhoInput](), Data: t[WhoData](),
	},
	{
		Name:     "why",
		Summary:  "Explain why a line, file or symbol is the way it is: the commit, the task behind it, and any agreement.",
		Args:     []Arg{{Name: "target", Required: true, Kind: KindString, Help: "a path, path:line, or a symbol"}},
		ReadOnly: true,
		Input:    t[WhyInput](), Data: t[WhyData](),
	},
	{
		Name:       "claim",
		Summary:    "You are changing the signature of an interface others use (function, method, type). Makes you its owner so others build against your version.",
		Args:       []Arg{{Name: "symbol", Required: true, Kind: KindString, Help: "[path:]Qualified"}, noteArg},
		Idempotent: true,
		Input:      t[ClaimInput](), Data: t[ClaimData](),
	},
	{
		Name:    "publish",
		Summary: "After claiming, publish the new signature so agents that depend on it can build on it before your work lands.",
		Args: []Arg{
			{Name: "symbol", Required: true, Kind: KindString, Help: "[path:]Qualified you own"},
			{Name: "commit", Kind: KindString, Help: "a stub commit to publish instead of the one gitlet builds"},
			noteArg,
		},
		Input: t[PublishInput](), Data: t[PublishData](),
	},
	{
		Name:    "need",
		Summary: "Your work depends on an interface someone else is changing. gitlet tells you when its new signature is published.",
		Args: []Arg{
			{Name: "symbol", Required: true, Kind: KindString, Help: "[path:]Qualified you depend on"},
			{Name: "requirement", Kind: KindString, Help: "what you need from it"},
			noteArg,
		},
		Idempotent: true,
		Input:      t[NeedInput](), Data: t[NeedData](),
	},
	{
		Name:        "use",
		Summary:     "A signature you need is published: bring it into your branch (cherry-picks the owner's stub) and pin it.",
		Args:        []Arg{{Name: "symbol", Required: true, Kind: KindString, Help: "[path:]Qualified you need"}, noteArg},
		Destructive: true,
		Input:       t[UseInput](), Data: t[UseData](),
	},
	{
		Name:       "release",
		Summary:    "Drop your claim or need on a symbol when you no longer change or depend on it.",
		Args:       []Arg{{Name: "symbol", Required: true, Kind: KindString, Help: "[path:]Qualified"}, noteArg},
		Idempotent: true,
		Input:      t[ReleaseInput](), Data: t[ReleaseData](),
	},
	{
		Name:    "discuss",
		Summary: "Talk to the other agents about a note or thread: ask what they need, or propose who owns an interface and its signature. Keep it short and concrete.",
		Args: []Arg{
			{Name: "about", Required: true, Kind: KindString, Help: "a note id (n-...) or thread id (t-...)"},
			{Name: "message", Required: true, Kind: KindString, Help: "what you want to say"},
			{Name: "proposal", Kind: KindObject, Help: "{owner: me|them|<agent>, signature, summary}"},
		},
		Input: t[DiscussInput](), Data: t[DiscussData](),
	},
	{
		Name:       "accept",
		Summary:    "Accept a proposal from a thread. When every participant accepted, gitlet applies it.",
		Args:       []Arg{{Name: "proposal", Required: true, Kind: KindString, Help: "proposal id (p-...)"}},
		Idempotent: true,
		Input:      t[AcceptInput](), Data: t[AcceptData](),
	},
	{
		Name:    "dismiss",
		Summary: "Close a note you will not act on: not_needed (real but harmless) or false_alarm (gitlet got it wrong).",
		Args: []Arg{
			{Name: "note", Required: true, Kind: KindString, Help: "note id (n-...)"},
			{Name: "as", Required: true, Kind: KindString, Help: "not_needed or false_alarm", Enum: []string{"not_needed", "false_alarm"}},
			{Name: "reason", Kind: KindString, Help: "why"},
		},
		Idempotent: true,
		Input:      t[DismissInput](), Data: t[DismissData](),
	},
	{
		Name:        "update",
		Summary:     "Bring your branch up to date with main. Predicts conflicts first, drops stub commits whose contracts landed, and never leaves a half-finished rebase.",
		Args:        []Arg{noteArg},
		Destructive: true,
		Input:       t[UpdateInput](), Data: t[UpdateData](),
	},
	{
		Name:    "submit",
		Summary: "When your work is ready: checks it against main and contracts, pushes your branch and opens a pull request. Use instead of git push and gh pr create. Never merges.",
		Args: []Arg{
			{Name: "title", Kind: KindString, Help: "pull request title"},
			{Name: "body", Kind: KindString, Help: "pull request body"},
			{Name: "draft", Kind: KindBool, Help: "open as a draft"},
			{Name: "symbol", Kind: KindString, Help: "open an interface-only PR for this published symbol"},
		},
		OpenWorld: true,
		Input:     t[SubmitInput](), Data: t[SubmitData](),
	},
	{
		Name:    "abandon",
		Summary: "Stop your current task: releases its claims and needs, deletes its snapshot ref and tells the others. Your branch is kept.",
		Args: []Arg{
			{Name: "reason", Kind: KindString, Help: "why"},
			{Name: "item", Kind: KindString, Help: "the task to stop, by its id from status; default: your current task"},
		},
		Destructive: true,
		Input:       t[AbandonInput](), Data: t[AbandonData](),
	},
}
