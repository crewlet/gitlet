package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/render"
)

// handler returns the tool handler for t. Failures come back as isError
// results the agent can read, never as protocol errors or panics.
func (s *Server) handler(t *tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("gitlet: panic in tool call", "tool", t.verb.Name, "panic", p, "stack", string(debug.Stack()))
				res, err = errorResult("gitlet hit an internal error in %s and logged it. Try again; if it repeats, run gitlet doctor.", t.verb.Name), nil
			}
		}()
		return s.callTool(ctx, t, req), nil
	}
}

// callTool decodes the arguments, picks the engine for the call's worktree
// and returns the verb's result: the reply as structured content, the text
// block as content, and isError for refusals.
func (s *Server) callTool(ctx context.Context, t *tool, req *mcp.CallToolRequest) *mcp.CallToolResult {
	var raw json.RawMessage
	if req.Params != nil {
		raw = req.Params.Arguments
	}
	in, err := t.decode(raw)
	if err != nil {
		return errorResult("%v", err)
	}
	dir := s.opt.Dir
	if w := worktreeOf(in); w != "" {
		if !filepath.IsAbs(w) {
			return errorResult("worktree must be an absolute path, got %q.", w)
		}
		dir = filepath.Clean(w)
	}
	eng, root, err := s.engineFor(ctx, dir, clientName(req.ClientInfo()))
	if err != nil {
		s.log.Warn("gitlet: cannot serve worktree", "dir", dir, "err", err)
		return openFailed(dir, err)
	}

	start := time.Now()
	out, err := eng.Call(ctx, t.verb.Name, in)
	r := envelope(out)
	if err == nil && r == nil {
		err = errors.New("the engine returned no result")
	}
	if err != nil {
		s.log.Error("gitlet: tool call failed", "tool", t.verb.Name, "worktree", root, "err", err)
		return errorResult("gitlet failed to run %s: %v. Try again; if it keeps failing, run gitlet doctor in %s.", t.verb.Name, err, root)
	}
	s.log.Debug("gitlet: tool call", "tool", t.verb.Name, "worktree", root, "outcome", r.Outcome, "took", time.Since(start))
	return &mcp.CallToolResult{
		StructuredContent: out,
		Content:           []mcp.Content{&mcp.TextContent{Text: render.Text(r, render.MCP)}},
		IsError:           r.Outcome == api.Refused,
	}
}

func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "error: " + fmt.Sprintf(format, args...)}},
	}
}

func openFailed(dir string, err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, errClosed):
		return errorResult("gitlet is shutting down.")
	case errors.Is(err, gitx.ErrNotRepo):
		return errorResult("%s is not inside a git work tree. Pass worktree with the absolute path of a git worktree.", dir)
	}
	return errorResult("gitlet cannot serve %s: %s. Run gitlet doctor there to check the setup.", dir, strings.TrimPrefix(err.Error(), "mcpserver: "))
}

// envelope returns out's result, or nil when out is nil or a nil pointer.
func envelope(out api.Output) *api.Result {
	if out == nil {
		return nil
	}
	if v := reflect.ValueOf(out); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	return out.Envelope()
}

// worktreeOf returns the worktree argument of a decoded input; every input
// embeds api.Where.
func worktreeOf(in any) string {
	v := reflect.Indirect(reflect.ValueOf(in))
	if v.Kind() != reflect.Struct {
		return ""
	}
	if f := v.FieldByName("Where"); f.IsValid() {
		if w, ok := f.Interface().(api.Where); ok {
			return w.Worktree
		}
	}
	return ""
}

// decode parses a tool call's arguments into a new input for the verb. Null
// counts as absent. Only the arguments the schema defines are accepted, the
// verb's required arguments must be present and non-empty, and the rest is
// checked against the schema before a strict decode.
func (t *tool) decode(raw json.RawMessage) (any, error) {
	obj := map[string]any{}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("arguments for %s must be a JSON object: %v", t.verb.Name, err)
		}
	}
	dropNulls(obj)
	if err := checkUnknown(t.verb.Name, obj, t.schema, ""); err != nil {
		return nil, err
	}
	if err := checkRequired(t.verb, obj); err != nil {
		return nil, err
	}
	if t.resolved != nil {
		if err := t.resolved.Validate(obj); err != nil {
			return nil, fmt.Errorf("invalid arguments for %s: %s", t.verb.Name, schemaError(err))
		}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: %s: re-encode arguments: %w", t.verb.Name, err)
	}
	in := t.verb.NewInput()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		return nil, fmt.Errorf("invalid arguments for %s: %v", t.verb.Name, err)
	}
	return in, nil
}

// schemaError shortens a validation error to the argument and the problem:
// "validating root: validating /properties/target: type: ..." becomes
// "target: type: ...".
func schemaError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "validating root: ")
	msg = strings.ReplaceAll(msg, "validating /properties/", "")
	return strings.ReplaceAll(msg, "/properties/", ".")
}

// dropNulls removes null members from obj and the objects inside it.
func dropNulls(obj map[string]any) {
	for k, v := range obj {
		switch v := v.(type) {
		case nil:
			delete(obj, k)
		case map[string]any:
			dropNulls(v)
		}
	}
}

// checkUnknown reports every member of obj that sch does not define, with
// the valid names, then looks inside object members. path is the dotted
// prefix of obj ("" at the top).
func checkUnknown(verb string, obj map[string]any, sch *jsonschema.Schema, path string) error {
	if sch == nil || len(sch.Properties) == 0 {
		return nil
	}
	var unknown []string
	for k := range obj {
		if _, ok := sch.Properties[k]; !ok {
			unknown = append(unknown, fmt.Sprintf("%q", path+k))
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		valid := sch.PropertyOrder
		if len(valid) == 0 {
			valid = slices.Sorted(maps.Keys(sch.Properties))
		}
		noun, what := "argument", "arguments"
		if len(unknown) > 1 {
			noun = "arguments"
		}
		if path != "" {
			what = "fields of " + strings.TrimSuffix(path, ".")
		}
		return fmt.Errorf("unknown %s %s for %s. Valid %s: %s.", noun, strings.Join(unknown, ", "), verb, what, strings.Join(valid, ", "))
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if sub, ok := obj[k].(map[string]any); ok {
			if err := checkUnknown(verb, sub, sch.Properties[k], path+k+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRequired reports the verb's required arguments that are missing or
// empty, with their help text.
func checkRequired(v api.Verb, obj map[string]any) error {
	var missing []string
	for _, a := range v.Args {
		if !a.Required {
			continue
		}
		val, ok := obj[a.Name]
		if s, isString := val.(string); ok && (!isString || strings.TrimSpace(s) != "") {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s)", a.Name, a.Help))
	}
	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("missing required argument for %s: %s.", v.Name, missing[0])
	}
	return fmt.Errorf("missing required arguments for %s: %s.", v.Name, strings.Join(missing, ", "))
}
