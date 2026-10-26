package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// clientEvents maps the claude-code subcommands to Claude Code's event names.
var clientEvents = map[string]string{
	"session-start":      "SessionStart",
	"user-prompt-submit": "UserPromptSubmit",
	"post-tool-use":      "PostToolUse",
}

// gitletToolPrefix names gitlet's own MCP tools in Claude Code. Their
// results already carry new notes.
const gitletToolPrefix = "mcp__gitlet__"

// maxEvent caps the event read from stdin. PostToolUse events carry the
// whole tool response, so the cap is generous.
const maxEvent = 32 << 20

// notesHeading heads the notes every hook prints.
const notesHeading = "new gitlet notes:"

// event is the part of a Claude Code hook event the hooks read. Unknown
// fields are ignored.
type event struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
}

// parseEvent decodes a hook event. Empty input is an empty event.
func parseEvent(data []byte) (event, error) {
	var ev event
	if len(bytes.TrimSpace(data)) == 0 {
		return ev, nil
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return event{}, fmt.Errorf("hook event: %w", err)
	}
	return ev, nil
}

// output is a Claude Code hook answer that adds context for the model.
type output struct {
	HookSpecificOutput specificOutput `json:"hookSpecificOutput"`
}

type specificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// encodeOutput returns the answer to an event as one line of JSON. HTML
// characters are not escaped: the text is for a model, not a web page.
func encodeOutput(eventName, text string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(output{specificOutput{HookEventName: eventName, AdditionalContext: text}})
	return b.Bytes(), err
}

// claudeCode answers a Claude Code hook (docs/SPEC.md, section 14.2) with
// new notes, or a short status at session start. It prints nothing when
// there is nothing to say.
func (r *runner) claudeCode(ctx context.Context, args []string) {
	if len(args) == 0 {
		r.usage()
		return
	}
	sub := args[0]
	name, ok := clientEvents[sub]
	if !ok {
		r.usage()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.clientBudget)
	defer cancel()

	data, err := readAll(ctx, r.stdin, maxEvent)
	if ctx.Err() != nil {
		r.report(ctx.Err())
		return
	}
	ev, perr := parseEvent(data)
	if err == nil {
		err = perr
	}
	if err != nil {
		// Carry on without the event: Claude Code starts hooks in the
		// project directory.
		r.report(err)
		ev = event{}
	}
	if sub == "post-tool-use" && strings.HasPrefix(ev.ToolName, gitletToolPrefix) {
		return
	}
	dir := ev.Cwd
	if dir == "" {
		if dir, err = r.getwd(); err != nil {
			r.report(err)
			return
		}
	}
	s, err := r.openSession(ctx, dir)
	if err != nil || s == nil {
		r.report(err)
		return
	}
	defer s.close()

	var text string
	if sub == "session-start" {
		text, err = r.sessionStart(ctx, s)
	} else {
		text, err = r.takeNotes(ctx, s)
	}
	if err != nil || text == "" {
		r.report(err)
		return
	}
	out, err := encodeOutput(name, text)
	if err == nil {
		_, err = r.stdout.Write(out)
	}
	r.report(err)
}

// takeNotes renders for the model the agent's new notes and the notes
// resolved since they were shown, and marks shown only the notes rendered
// in full: the rest stay new for the next hook or result.
//
// When every new note fits in full, TakeUpdates delivers them with the
// resolutions in one transaction, so hooks running side by side (Claude
// Code runs parallel tool calls) never print the same note. A backlog is
// marked shown by id instead, and its resolutions wait for a later hook.
// A read-only ledger (a newer gitlet's) shows nothing new.
func (r *runner) takeNotes(ctx context.Context, s *session) (string, error) {
	if s.agent == "" || s.ledger.ReadOnly() {
		return "", nil
	}
	fresh, err := s.newNotes(ctx, "")
	if err != nil {
		return "", err
	}
	if len(fresh) > render.MaxFullNotes {
		text := notesText(fresh, nil, render.MCP)
		if text != "" {
			s.markShown(ctx, shownInFull(fresh), r.now())
		}
		return text, nil
	}
	fresh, resolved, err := s.ledger.TakeUpdates(ctx, s.agent, r.now())
	if err != nil {
		return "", err
	}
	return notesText(fresh, resolved, render.MCP), nil
}

// notesText renders new notes under the hooks' heading (render.Notes caps
// how many appear in full), then a line per resolved note, at most
// render.MaxFullNotes of them. It is "" when there is nothing to say.
func notesText(fresh, resolved []model.Note, mode render.Mode) string {
	var b strings.Builder
	if len(fresh) > 0 {
		if t := strings.TrimRight(render.Notes(notesHeading, fresh, mode), "\n"); t != "" {
			b.WriteString(t + "\n")
		}
	}
	for i, n := range resolved {
		if i == render.MaxFullNotes {
			fmt.Fprintf(&b, "and %d more resolved gitlet notes.\n", len(resolved)-i)
			break
		}
		b.WriteString("resolved gitlet note: " + oneLine(n.Title) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// sessionStart returns the short status a session starts with: how many
// notes are open, and what to call first. It shows no notes: they stay new
// for the next hook or result.
func (r *runner) sessionStart(ctx context.Context, s *session) (string, error) {
	var open, act int
	var items []model.WorkItem
	var branch string
	itemKnown := true
	if s.agent != "" {
		var err error
		if open, act, err = s.ledger.Counts(ctx, s.agent); err != nil {
			return "", err
		}
		if items, branch, err = agentItems(ctx, s.repo, s.agent); err != nil {
			r.report(err)
			itemKnown = false
		}
	}
	// An active item on another branch is still the worktree's task, and
	// start would refuse to open a second one: name it.
	item := pickItem(items, branch)
	if item == nil {
		item = soleActive(items)
	}
	return sessionStatus(open, act, item, branch, itemKnown), nil
}

// sessionStatus words the session-start status. branch is the checked-out
// branch, and itemKnown is false when the agent's state could not be read.
func sessionStatus(open, act int, item *model.WorkItem, branch string, itemKnown bool) string {
	var b strings.Builder
	switch open {
	case 0:
		b.WriteString("gitlet: no open notes.\n")
	case 1:
		fmt.Fprintf(&b, "gitlet: 1 open note (%d act).\n", act)
	default:
		fmt.Fprintf(&b, "gitlet: %d open notes (%d act).\n", open, act)
	}
	switch {
	case item != nil && item.Branch != "" && item.Branch != branch:
		fmt.Fprintf(&b, "Your task %q is on branch %s, not the one checked out: switch back to it, or call status to see it and its notes.",
			clip(item.Intent, 100), item.Branch)
	case item != nil && item.Provisional && item.Branch == "":
		b.WriteString("Your work has no stated task: call start with a one-sentence intent.")
	case item != nil && item.Provisional:
		fmt.Fprintf(&b, "Your work on branch %s has no stated task: call start with a one-sentence intent.", item.Branch)
	case item != nil:
		fmt.Fprintf(&b, "Your task: %q", clip(item.Intent, 100))
		if item.Branch != "" {
			b.WriteString(" on branch " + item.Branch)
		}
		b.WriteString(". Call status to see it, what overlaps it and every open note.")
	case itemKnown && open > 0:
		b.WriteString("No task started in this worktree: call status to see the notes, and start with a one-sentence intent before you edit code.")
	case itemKnown:
		b.WriteString("No task started in this worktree: call start with a one-sentence intent before you edit code.")
	default:
		b.WriteString("Call status to see your task and notes, or start with a one-sentence intent before you edit code.")
	}
	return b.String()
}

// oneLine joins the lines of s with spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip returns s on one line, cut to at most n runes.
func clip(s string, n int) string {
	s = oneLine(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}
