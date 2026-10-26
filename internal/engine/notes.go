package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
	"github.com/crewlet/gitlet/internal/rules"
)

// maxSkippedWarnings caps the skipped-document warnings in one result.
const maxSkippedWarnings = 5

// noteStore is the ledger's note API (docs/SPEC.md, section 10.1). It is an
// interface so result decoration can be tested with a fake.
type noteStore interface {
	SyncNotes(ctx context.Context, agent model.AgentID, notes []model.Note, now time.Time) error
	TakeUpdates(ctx context.Context, agent model.AgentID, now time.Time) (fresh, resolved []model.Note, err error)
	OpenNotes(ctx context.Context, agent model.AgentID) ([]ledger.StoredNote, error)
	MarkShown(ctx context.Context, agent model.AgentID, ids []string, now time.Time) error
	Counts(ctx context.Context, agent model.AgentID) (open, openAct int, err error)
	Note(ctx context.Context, agent model.AgentID, idOrPrefix string) (ledger.StoredNote, error)
	Acted(ctx context.Context, agent model.AgentID, id string, now time.Time) error
}

// evaluate runs the rules over the full view (costly parts within budget)
// and, when this process delivers the agent's notes (syncsNotes), reconciles
// them in the ledger and records new signature clashes. The view and the
// produced notes stay in e.lastView and e.lastNotes for the rest of the
// verb. Callers hold e.mu.
func (e *Engine) evaluate(ctx context.Context, budget time.Duration) (rules.View, []model.Note, error) {
	v := e.fullView(ctx, budget)
	notes := rules.Evaluate(v)
	e.lastView, e.lastNotes = &v, notes
	if !e.syncsNotes() {
		return v, notes, nil
	}
	if err := e.notes.SyncNotes(ctx, e.me.ID, notes, v.Now); err != nil {
		return v, notes, fmt.Errorf("engine: sync notes: %w", err)
	}
	e.recordClashes(ctx, v)
	return v, notes, nil
}

// finish completes every verb's result (docs/SPEC.md, section 4.2): when
// the verb did not refuse and was acting on a note (its note argument), the
// note is marked acted; the rules run again so the result reflects what the
// verb changed; then decorate fills notes, counts, state age and warnings.
// finish never fails, problems become warnings. Verbs call it last, while
// holding the engine (between beginVerb and its release).
func (e *Engine) finish(ctx context.Context, r *api.Result, note string) {
	if note != "" && r.Outcome != api.Refused {
		e.actOnNote(ctx, r, note)
	}
	if _, _, err := e.evaluate(ctx, verbEvalBudget); err != nil {
		e.log.Debug("evaluate", "err", err)
		r.Warnings = append(r.Warnings, "notes not updated: "+err.Error())
	}
	e.decorate(ctx, r, false)
}

// actOnNote marks the note a verb acted on (docs/SPEC.md, section 4.1).
func (e *Engine) actOnNote(ctx context.Context, r *api.Result, note string) {
	if e.sameWorktree {
		return
	}
	sn, err := e.notes.Note(ctx, e.me.ID, note)
	if err == nil {
		err = e.notes.Acted(ctx, e.me.ID, sn.Note.ID, e.now())
	}
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("note %s not marked resolved: %v", note, err))
	}
}

// decorate fills the envelope of r: notes (new notes capped at
// render.MaxFullNotes, or every open note when all), counts, state age,
// offline time and warnings, and makes sure a refusal offers a way out.
func (e *Engine) decorate(ctx context.Context, r *api.Result, all bool) {
	now := e.now()
	if r.Outcome == "" {
		r.Outcome = api.OK
	}
	if e.sameWorktree {
		// The worktree's live session delivers this agent's notes; this one
		// only says why it is read-only, and status may look without marking.
		r.Notes = notesOfKind(e.lastNotes, model.NoteSameWorktree)
		if all {
			if stored, err := e.notes.OpenNotes(ctx, e.me.ID); err == nil {
				for _, s := range stored {
					r.Notes = append(r.Notes, s.Note)
				}
			}
			r.AllNotes = true
		}
		if open, act, err := e.notes.Counts(ctx, e.me.ID); err == nil {
			r.Open, r.OpenAct = open, act
		}
	} else if err := decorateNotes(ctx, e.notes, e.me.ID, now, r, all); err != nil {
		e.log.Debug("decorate notes", "err", err)
		r.Warnings = append(r.Warnings, "notes unavailable: "+err.Error())
	}
	r.StateAge, r.OfflineSince = e.stateAge(ctx, now)
	r.Warnings = append(r.Warnings, e.verbWarnings...)
	r.Warnings = dedupe(append(r.Warnings, e.warnings(r.OfflineSince, r.StateAge)...))
	if r.Outcome == api.Refused && len(r.Options) == 0 {
		r.Options = []model.Option{statusOption()}
	}
}

// decorateNotes fills r's note fields from the ledger. For results other
// than status it shows at most render.MaxFullNotes new notes in severity
// order, counts the rest in MoreNotes and marks only the shown ones;
// resolved titles come along when nothing beyond those is pending, because
// TakeUpdates hands out every new note at once. With all (status) it lists
// every new and open note and marks exactly those it lists. A read-only
// ledger is read without marking.
func decorateNotes(ctx context.Context, store noteStore, agent model.AgentID, now time.Time, r *api.Result, all bool) error {
	var err error
	if all {
		err = statusNotes(ctx, store, agent, now, r)
	} else {
		err = freshNotes(ctx, store, agent, now, r)
	}
	if err != nil {
		return err
	}
	open, act, err := store.Counts(ctx, agent)
	if err != nil {
		return fmt.Errorf("engine: count notes: %w", err)
	}
	r.Open, r.OpenAct = open, act
	return nil
}

func freshNotes(ctx context.Context, store noteStore, agent model.AgentID, now time.Time, r *api.Result) error {
	stored, err := store.OpenNotes(ctx, agent)
	if err != nil {
		return fmt.Errorf("engine: open notes: %w", err)
	}
	pending := newNotes(stored)
	if len(pending) > render.MaxFullNotes {
		shown := pending[:render.MaxFullNotes]
		if err := store.MarkShown(ctx, agent, noteIDs(shown), now); err != nil && !errors.Is(err, ledger.ErrReadOnly) {
			return fmt.Errorf("engine: mark notes shown: %w", err)
		}
		r.Notes, r.MoreNotes = shown, len(pending)-len(shown)
		return nil
	}
	fresh, resolved, err := store.TakeUpdates(ctx, agent, now)
	switch {
	case errors.Is(err, ledger.ErrReadOnly):
		fresh, resolved = pending, nil
	case err != nil:
		return fmt.Errorf("engine: take note updates: %w", err)
	}
	r.Notes, r.MoreNotes = capNotes(fresh, render.MaxFullNotes)
	r.Resolved = noteTitles(resolved)
	return nil
}

func statusNotes(ctx context.Context, store noteStore, agent model.AgentID, now time.Time, r *api.Result) error {
	stored, err := store.OpenNotes(ctx, agent)
	if err != nil {
		return fmt.Errorf("engine: open notes: %w", err)
	}
	notes := make([]model.Note, 0, len(stored))
	for _, s := range stored {
		notes = append(notes, s.Note)
	}
	readOnly := false
	if len(notes) > 0 {
		err := store.MarkShown(ctx, agent, noteIDs(notes), now)
		switch {
		case errors.Is(err, ledger.ErrReadOnly):
			readOnly = true
		case err != nil:
			return fmt.Errorf("engine: mark notes shown: %w", err)
		}
	}
	if !readOnly {
		// Notes that appeared since OpenNotes come back as fresh: list them
		// too, since TakeUpdates just marked them shown.
		fresh, resolved, err := store.TakeUpdates(ctx, agent, now)
		switch {
		case errors.Is(err, ledger.ErrReadOnly):
		case err != nil:
			return fmt.Errorf("engine: take note updates: %w", err)
		default:
			seen := map[string]bool{}
			for _, n := range notes {
				seen[n.ID] = true
			}
			for _, n := range fresh {
				if !seen[n.ID] {
					notes = append(notes, n)
				}
			}
			r.Resolved = noteTitles(resolved)
		}
	}
	sortNotes(notes)
	r.Notes, r.MoreNotes, r.AllNotes = notes, 0, true
	return nil
}

// newNotes returns the stored notes not shown yet, in severity order.
func newNotes(stored []ledger.StoredNote) []model.Note {
	var out []model.Note
	for _, s := range stored {
		if s.Status == ledger.NoteNew {
			out = append(out, s.Note)
		}
	}
	sortNotes(out)
	return out
}

// capNotes orders notes by severity (stable otherwise) and splits them
// into the max shown in full and the number left over.
func capNotes(notes []model.Note, max int) (shown []model.Note, more int) {
	notes = append([]model.Note(nil), notes...)
	sortNotes(notes)
	if len(notes) <= max {
		return notes, 0
	}
	return notes[:max], len(notes) - max
}

// sortNotes orders notes act, warn, info, keeping the order within a severity.
func sortNotes(notes []model.Note) {
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].Severity.Rank() < notes[j].Severity.Rank() })
}

func noteIDs(notes []model.Note) []string {
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	return ids
}

func noteTitles(notes []model.Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Title)
	}
	return out
}

func notesOfKind(notes []model.Note, kind model.NoteKind) []model.Note {
	var out []model.Note
	for _, n := range notes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// warnings lists what every result says about the engine's own condition:
// offline time, old git, read-only reasons, skipped documents, rejected
// snapshots and items whose base is too far behind main.
func (e *Engine) warnings(offline *time.Time, age string) []string {
	var w []string
	if offline != nil {
		if age == "" {
			age = "an unknown time"
		}
		w = append(w, fmt.Sprintf("offline since %s: using state from %s ago", offline.Local().Format("15:04"), age))
	}
	if e.gitWarn != "" {
		w = append(w, e.gitWarn)
	}
	if reason := e.readOnlyReason(); reason != "" {
		w = append(w, "read-only: "+reason)
	}
	if e.shared != nil {
		skipped := append([]string(nil), e.shared.Skipped...)
		sort.Strings(skipped)
		for i, s := range skipped {
			if i == maxSkippedWarnings {
				w = append(w, fmt.Sprintf("%d more documents skipped", len(skipped)-i))
				break
			}
			w = append(w, "skipped "+s)
		}
	}
	items := e.myState().Items
	for _, it := range items {
		if it.Status.Open() && it.SnapshotErr != "" {
			w = append(w, fmt.Sprintf("the remote rejected the snapshot of %q: %s", it.Intent, it.SnapshotErr))
		}
	}
	if e.lastView != nil {
		for _, it := range items {
			if mv, ok := e.lastView.Main[it.ID]; ok && mv.Skipped {
				w = append(w, fmt.Sprintf("main changed more than %d files since the base of %q, so it is not checked symbol by symbol: run update", mainParseLimit, it.Intent))
			}
		}
	}
	return w
}

// dedupe drops repeated strings, keeping the first occurrence.
func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---- result builders ----

// okResult builds a successful result.
func okResult(summary string, details ...string) api.Result {
	return api.Result{Outcome: api.OK, Summary: summary, Details: details}
}

// nothingToDo builds a nothing_to_do result.
func nothingToDo(summary string, details ...string) api.Result {
	return api.Result{Outcome: api.NothingToDo, Summary: summary, Details: details}
}

// refused builds a refusal. Refusals always carry at least one way
// forward: without options it offers status.
func refused(summary string, details []string, opts ...model.Option) api.Result {
	if len(opts) == 0 {
		opts = []model.Option{statusOption()}
	}
	return api.Result{Outcome: api.Refused, Summary: summary, Details: details, Options: opts}
}

// toolOption is an option calling a gitlet verb, with arguments given as
// key/value pairs: toolOption("wait for their signature", "need",
// "symbol", "src/a.ts:f", "note", id). Empty string values are left out.
func toolOption(label, tool string, kv ...any) model.Option {
	o := model.Option{Label: label, Tool: tool}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if s, isString := kv[i+1].(string); k == "" || (isString && s == "") {
			continue
		}
		if o.Args == nil {
			o.Args = map[string]any{}
		}
		o.Args[k] = kv[i+1]
	}
	return o
}

// cmdOption is an option running a shell command.
func cmdOption(label, command string) model.Option {
	return model.Option{Label: label, Command: command}
}

func statusOption() model.Option {
	return toolOption("see your task, the work around it and every open note", "status")
}

// reply wraps a result and verb data.
func reply[D any](r api.Result, data D) *api.Reply[D] {
	return &api.Reply[D]{Result: r, Data: data}
}

// readOnlyRefusal refuses a verb that would publish while the engine is
// read-only, with the way out that fits the reason.
func (e *Engine) readOnlyRefusal(verb string) api.Result {
	summary := fmt.Sprintf("Cannot %s: this gitlet session is read-only (%s).", verb, e.readOnlyReason())
	switch {
	case e.sameWorktree:
		return refused(summary, []string{"one agent per worktree: give this session its own worktree"},
			cmdOption("create a worktree of your own, then work there", e.worktreeCmd))
	case e.observer.Load():
		return refused(summary, []string{"the remote refused a push of refs/gitlet/*"},
			cmdOption("check what the remote allows", "gitlet doctor"), statusOption())
	case e.collision.Load():
		return refused(summary, []string{"another process pushed this agent's refs twice under us"},
			cmdOption("check the setup", "gitlet doctor"), statusOption())
	}
	return refused(summary, nil, cmdOption("check the setup", "gitlet doctor"), statusOption())
}

// writeRefusal turns an error from mutateState, mutateOutbox or a claim
// write into a refusal when it is one: a read-only engine, an unreachable
// remote (claims need it), a claim held or changed by someone else, or a
// push the remote rejected. ok is false for real failures, which the verb
// returns as errors.
func (e *Engine) writeRefusal(verb string, err error) (api.Result, bool) {
	var rej *bus.RejectedError
	switch {
	case err == nil:
		return api.Result{}, false
	case errors.Is(err, errReadOnly):
		return e.readOnlyRefusal(verb), true
	case errors.Is(err, bus.ErrUnreachable), gitx.IsNetwork(err):
		return refused(fmt.Sprintf("Cannot %s: remote unreachable; claims need the remote.", verb), nil,
			toolOption("try again once the remote is back", verb), statusOption()), true
	case errors.Is(err, bus.ErrClaimLost):
		return refused(fmt.Sprintf("Cannot %s: the claim is held or was changed by another agent.", verb), nil,
			statusOption()), true
	case errors.Is(err, bus.ErrObserver):
		e.enterObserver()
		return e.readOnlyRefusal(verb), true
	case errors.Is(err, bus.ErrTooLarge):
		return refused(fmt.Sprintf("Cannot %s: the document would exceed %d KiB.", verb, bus.MaxWriteDoc>>10), nil,
			statusOption()), true
	case errors.As(err, &rej):
		details := append([]string{rej.Reason}, rej.Remote...)
		return refused(fmt.Sprintf("Cannot %s: the remote rejected the push of %s.", verb, rej.Ref), details,
			cmdOption("check what the remote allows", "gitlet doctor")), true
	}
	return api.Result{}, false
}
