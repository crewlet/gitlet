package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/model"
)

var errClosed = errors.New("engine: closed")

// beginVerb starts a verb: it takes the engine (verbs and Tick are
// serialized), records activity, refreshes the shared state with the
// verb-time rule (refreshForVerb) and, with snap, takes a snapshot first
// (snapshotNow), as start, status, claim, publish and submit must
// (docs/SPEC.md, section 9.1). A failed snapshot becomes a warning on the
// result, not an error. Call the returned release with defer:
//
//	release, err := e.beginVerb(ctx, true)
//	if err != nil {
//		return nil, err
//	}
//	defer release()
//	... build r ...
//	e.finish(ctx, &r.Result, in.Note)
//	return r, nil
func (e *Engine) beginVerb(ctx context.Context, snap bool) (release func(), err error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errClosed
	}
	e.verbWarnings = nil
	e.touch(ctx, e.now())
	if err := e.refreshForVerb(ctx); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if snap {
		if err := e.snapshotNow(ctx); err != nil {
			e.log.Debug("snapshot", "err", err)
			e.verbWarnings = append(e.verbWarnings, "snapshot failed: "+err.Error())
		}
	}
	return e.mu.Unlock, nil
}

// handler runs one verb with its decoded input, a pointer to the verb's
// input type.
type handler func(e *Engine, ctx context.Context, in any) (api.Output, error)

// bind adapts a typed verb method to a handler.
func bind[I, D any](f func(*Engine, context.Context, I) (*api.Reply[D], error)) handler {
	return func(e *Engine, ctx context.Context, in any) (api.Output, error) {
		r, err := f(e, ctx, *in.(*I))
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, errors.New("engine: verb returned no result")
		}
		return r, nil
	}
}

// handlerFor returns the method behind a verb of api.Verbs.
func handlerFor(verb string) handler {
	switch verb {
	case "start":
		return bind((*Engine).Start)
	case "status":
		return bind((*Engine).Status)
	case "who":
		return bind((*Engine).Who)
	case "why":
		return bind((*Engine).Why)
	case "claim":
		return bind((*Engine).Claim)
	case "publish":
		return bind((*Engine).Publish)
	case "need":
		return bind((*Engine).Need)
	case "use":
		return bind((*Engine).Use)
	case "release":
		return bind((*Engine).Release)
	case "discuss":
		return bind((*Engine).Discuss)
	case "accept":
		return bind((*Engine).Accept)
	case "dismiss":
		return bind((*Engine).Dismiss)
	case "update":
		return bind((*Engine).Update)
	case "submit":
		return bind((*Engine).Submit)
	case "abandon":
		return bind((*Engine).Abandon)
	}
	return nil
}

func (e *Engine) call(ctx context.Context, verb string, input any) (api.Output, error) {
	v, ok := api.Lookup(verb)
	h := handlerFor(verb)
	if !ok || h == nil {
		names := make([]string, len(api.Verbs))
		for i, v := range api.Verbs {
			names[i] = v.Name
		}
		return e.usageRefusal(ctx, refused(fmt.Sprintf("Unknown verb %q.", verb),
			[]string{"verbs: " + strings.Join(names, ", ")}, statusOption()))
	}
	in, err := decodeInput(v, input)
	var ue *usageError
	if errors.As(err, &ue) {
		return e.usageRefusal(ctx, refused(fmt.Sprintf("Invalid arguments for %s: %s.", v.Name, ue.msg),
			usageDetails(v), usageOption(v)))
	}
	if err != nil {
		return nil, err
	}
	if w, ok := whereOf(in); ok {
		if r := e.worktreeRefusal(v.Name, w); r != nil {
			return e.usageRefusal(ctx, *r)
		}
	}
	return h(e, ctx, in)
}

// whereOf returns the api.Where an input embeds.
func whereOf(in any) (api.Where, bool) {
	rv := reflect.ValueOf(in)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return api.Where{}, false
	}
	f := rv.Elem().FieldByName("Where")
	if !f.IsValid() {
		return api.Where{}, false
	}
	w, ok := f.Interface().(api.Where)
	return w, ok
}

// worktreeRefusal checks a call's worktree argument: empty, this engine's
// worktree root, or a directory inside it that belongs to no other
// worktree. The MCP server routes calls to the engine of their worktree;
// an engine never re-routes, it refuses a call meant for another worktree.
// Call checks every verb; verbs check it too when called directly.
func (e *Engine) worktreeRefusal(verb string, w api.Where) *api.Result {
	dir := strings.TrimSpace(w.Worktree)
	if dir == "" || insideWorktree(e.repo.Root, dir) {
		return nil
	}
	r := refused(fmt.Sprintf("Cannot %s here: %s is not inside this worktree (%s).", verb, dir, e.repo.Root), nil,
		toolOption("look at this worktree instead", "status", "worktree", e.repo.Root))
	return &r
}

// insideWorktree reports whether dir is root or a directory inside it that
// is not part of a nested repository or worktree (a .git entry on the way).
func insideWorktree(root, dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	for d := abs; d != root && len(d) > len(root); d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return false
		}
	}
	return true
}

// usageRefusal decorates a refusal that never reached a verb.
func (e *Engine) usageRefusal(ctx context.Context, r api.Result) (api.Output, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errClosed
	}
	e.verbWarnings = nil
	e.decorate(ctx, &r, false)
	return &r, nil
}

// usageError is input that does not fit a verb's arguments.
type usageError struct{ msg string }

func (u *usageError) Error() string { return u.msg }

// decodeInput turns Call's input into a pointer to the verb's input type:
// nil (no arguments), a pointer to the input type, the input value itself,
// a map of arguments, or JSON (json.RawMessage or []byte) whose unknown
// fields are rejected. Ill-formed JSON is a *usageError; an input of
// another Go type is a programming error.
func decodeInput(v api.Verb, input any) (any, error) {
	ptr := v.NewInput()
	switch in := input.(type) {
	case nil:
		return ptr, nil
	case json.RawMessage:
		return ptr, decodeJSON(in, ptr)
	case []byte:
		return ptr, decodeJSON(in, ptr)
	case map[string]any:
		b, err := json.Marshal(in)
		if err != nil {
			return nil, &usageError{err.Error()}
		}
		return ptr, decodeJSON(b, ptr)
	}
	rv := reflect.ValueOf(input)
	switch rv.Type() {
	case reflect.PointerTo(v.Input):
		if rv.IsNil() {
			return ptr, nil
		}
		return input, nil
	case v.Input:
		reflect.ValueOf(ptr).Elem().Set(rv)
		return ptr, nil
	}
	return nil, fmt.Errorf("engine: %s: input of type %T, want %s, *%s, a map or JSON", v.Name, input, v.Input, v.Input)
}

func decodeJSON(b []byte, dst any) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &usageError{strings.TrimPrefix(err.Error(), "json: ")}
	}
	if dec.More() {
		return &usageError{"unexpected data after the arguments"}
	}
	return nil
}

// usageDetails lists a verb's arguments for a usage refusal.
func usageDetails(v api.Verb) []string {
	var out []string
	for _, a := range v.Args {
		req := "optional"
		if a.Required {
			req = "required"
		}
		line := fmt.Sprintf("%s (%s, %s): %s", a.Name, a.Kind, req, a.Help)
		if len(a.Enum) > 0 {
			line += " [" + strings.Join(a.Enum, ", ") + "]"
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		out = append(out, v.Name+" takes no arguments")
	}
	return out
}

// usageOption is an example call of a verb with its required arguments.
func usageOption(v api.Verb) model.Option {
	o := model.Option{Label: "call it like this", Tool: v.Name}
	for _, a := range v.Args {
		if a.Required {
			if o.Args == nil {
				o.Args = map[string]any{}
			}
			o.Args[a.Name] = "<" + a.Help + ">"
		}
	}
	return o
}
