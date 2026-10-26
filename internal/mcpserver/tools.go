package mcpserver

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
)

// tool is one verb exposed as an MCP tool.
type tool struct {
	verb     api.Verb
	def      *mcp.Tool
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved // nil when the schema could not be resolved
}

// titles are the human-readable tool names clients may show.
var titles = map[string]string{
	"start":   "Start a task",
	"status":  "Show status",
	"who":     "Who works on this",
	"why":     "Why it is this way",
	"claim":   "Claim an interface",
	"publish": "Publish a signature",
	"need":    "Need an interface",
	"use":     "Use a published signature",
	"release": "Release a claim or need",
	"discuss": "Discuss with other agents",
	"accept":  "Accept a proposal",
	"dismiss": "Dismiss a note",
	"update":  "Update from main",
	"submit":  "Submit for review",
	"abandon": "Abandon the task",
}

func title(v api.Verb) string {
	if t, ok := titles[v.Name]; ok {
		return t
	}
	return "gitlet " + v.Name
}

// buildTools derives one tool per verb, in table order.
func buildTools(log *slog.Logger) []*tool {
	tools := make([]*tool, 0, len(api.Verbs))
	for _, v := range api.Verbs {
		t := &tool{verb: v}
		sch, err := inputSchema(v)
		if err != nil {
			// Not reachable with the current table (the tests build every
			// schema); serve the tool with a permissive schema rather than none.
			log.Error("gitlet: cannot build input schema", "tool", v.Name, "err", err)
			sch = &jsonschema.Schema{Type: "object"}
		}
		t.schema = sch
		if t.resolved, err = sch.Resolve(nil); err != nil {
			log.Error("gitlet: cannot resolve input schema", "tool", v.Name, "err", err)
		}
		t.def = &mcp.Tool{
			Name:        v.Name,
			Title:       title(v),
			Description: v.Summary,
			InputSchema: sch,
			Annotations: annotations(v),
		}
		tools = append(tools, t)
	}
	return tools
}

// inputSchema generates the JSON schema of a verb's input type: properties
// in field order with the descriptions from the jsonschema tags, falling back
// to the verb table's help, enums from the table, and no "null" alternatives
// (an optional argument is simply left out).
func inputSchema(v api.Verb) (*jsonschema.Schema, error) {
	sch, err := jsonschema.ForType(v.Input, nil)
	if err != nil {
		return nil, err
	}
	dropNull(sch)
	for _, a := range v.Args {
		p := sch.Properties[a.Name]
		if p == nil {
			continue
		}
		if p.Description == "" {
			p.Description = a.Help
		}
		if len(a.Enum) > 0 {
			p.Enum = make([]any, len(a.Enum))
			for i, e := range a.Enum {
				p.Enum[i] = e
			}
		}
	}
	return sch, nil
}

// dropNull turns {"type": ["null", T]} into {"type": T} throughout s. Some
// clients reject type arrays, and the handler treats null as absent anyway.
func dropNull(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) == 2 && s.Types[0] == "null" {
		s.Type, s.Types = s.Types[1], nil
	}
	for _, p := range s.Properties {
		dropNull(p)
	}
	dropNull(s.Items)
}

func annotations(v api.Verb) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title(v),
		ReadOnlyHint:    v.ReadOnly,
		IdempotentHint:  v.Idempotent,
		DestructiveHint: ptr(v.Destructive),
		OpenWorldHint:   ptr(v.OpenWorld),
	}
}

func ptr[T any](v T) *T { return &v }

// toolOrder lists tools in verb table order. The SDK keeps tools in a set
// and lists them by name; start, status and who belong first.
func toolOrder(tools []*tool) mcp.Middleware {
	rank := make(map[string]int, len(tools))
	for i, t := range tools {
		rank[t.verb.Name] = i
	}
	byRank := func(a, b *mcp.Tool) int {
		ra, oka := rank[a.Name]
		rb, okb := rank[b.Name]
		switch {
		case oka && okb:
			return cmp.Compare(ra, rb)
		case oka != okb: // gitlet's tools before any others
			if oka {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				slices.SortStableFunc(r.Tools, byRank)
			}
			return res, err
		}
	}
}
