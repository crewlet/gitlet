package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// examples show one call per verb in the spec's style. A verb missing here
// gets an example built from its required arguments.
var examples = map[string]string{
	"start":   `gitlet start "Add strict mode to config loading"`,
	"status":  `gitlet status`,
	"who":     `gitlet who src/config.ts:parseConfig`,
	"why":     `gitlet why src/config.ts:42`,
	"claim":   `gitlet claim src/config.ts:parseConfig --note n-3f2a`,
	"publish": `gitlet publish src/config.ts:parseConfig`,
	"need":    `gitlet need src/config.ts:parseConfig --requirement "keep returning Config"`,
	"use":     `gitlet use src/config.ts:parseConfig --note n-3f2a`,
	"release": `gitlet release src/config.ts:parseConfig`,
	"discuss": `gitlet discuss n-3f2a "Do you need the flag on parseConfig?"`,
	"accept":  `gitlet accept p-7c1d`,
	"dismiss": `gitlet dismiss n-3f2a not_needed --reason "only a comment"`,
	"update":  `gitlet update`,
	"submit":  `gitlet submit --title "Add strict mode to config loading" --draft`,
	"abandon": `gitlet abandon --reason "duplicate of codex-ci-1a2b3c's work"`,
}

// extraExamples are shown in help after the main example.
var extraExamples = map[string][]string{
	"discuss": {`gitlet discuss t-8c1f "I can own it" --proposal '{"owner":"me","signature":"parseConfig(text: string, strict?: boolean): Config","summary":"I own parseConfig and add an optional strict flag"}'`},
	"submit":  {`gitlet submit --symbol src/config.ts:parseConfig`},
}

func example(v api.Verb) string {
	if ex, ok := examples[v.Name]; ok {
		return ex
	}
	parts := []string{"gitlet", v.Name}
	for _, a := range v.Args {
		if a.Required {
			parts = append(parts, "<"+a.Name+">")
		}
	}
	return strings.Join(parts, " ")
}

// subField is one field of an object argument. An object argument is one
// flag holding a JSON object, the form render writes in options:
// --proposal '{"owner":"me","summary":"..."}'.
type subField struct {
	name     string // JSON name
	required bool
	help     string
}

// objectFields describes the struct behind an object argument of v.
func objectFields(v api.Verb, arg api.Arg) []subField {
	f, ok := fieldByJSONName(v.Input, arg.Name)
	if !ok {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []subField
	for _, sf := range reflect.VisibleFields(t) {
		name, omitempty, ok := jsonName(sf)
		if !ok || sf.Anonymous {
			continue
		}
		out = append(out, subField{name: name, required: !omitempty, help: sf.Tag.Get("jsonschema")})
	}
	return out
}

// fieldByJSONName finds the field of struct t whose JSON name is name.
func fieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for _, sf := range reflect.VisibleFields(t) {
		if n, _, ok := jsonName(sf); ok && !sf.Anonymous && n == name {
			return sf, true
		}
	}
	return reflect.StructField{}, false
}

func jsonName(sf reflect.StructField) (name string, omitempty, ok bool) {
	if !sf.IsExported() {
		return "", false, false
	}
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = sf.Name
	}
	return name, strings.Contains(","+opts+",", ",omitempty,"), true
}

func splitArgs(v api.Verb) (required, optional []api.Arg) {
	for _, a := range v.Args {
		if a.Required {
			required = append(required, a)
		} else {
			optional = append(optional, a)
		}
	}
	return required, optional
}

// placeholder is how a positional argument appears in usage lines.
func placeholder(a api.Arg) string { return "<" + a.Name + ">" }

// usageLine is the full correct form of a verb call, every flag included.
func usageLine(v api.Verb) string {
	req, opt := splitArgs(v)
	parts := []string{"gitlet", v.Name}
	for _, a := range req {
		parts = append(parts, placeholder(a))
	}
	for _, a := range opt {
		switch a.Kind {
		case api.KindBool:
			parts = append(parts, "[--"+a.Name+"]")
		case api.KindStrings:
			parts = append(parts, fmt.Sprintf("[--%s <%s>,...]", a.Name, strings.TrimSuffix(a.Name, "s")))
		case api.KindObject:
			parts = append(parts, fmt.Sprintf("[--%s <json>]", a.Name))
		default:
			parts = append(parts, fmt.Sprintf("[--%s <%s>]", a.Name, a.Name))
		}
	}
	parts = append(parts, "[--worktree <dir>]", "[--json]")
	return strings.Join(parts, " ")
}

// longHelp is the summary, the positional arguments with their help, and
// how to give object arguments.
func longHelp(v api.Verb) string {
	paras := []string{wrap(v.Summary, helpWidth, 0)}
	req, opt := splitArgs(v)
	if len(req) > 0 {
		width := 0
		for _, a := range req {
			width = max(width, len(placeholder(a)))
		}
		lines := []string{"Arguments:"}
		for _, a := range req {
			lines = append(lines, fmt.Sprintf("  %-*s  %s", width, placeholder(a), a.Help))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}
	for _, a := range opt {
		if a.Kind != api.KindObject {
			continue
		}
		fields := objectFields(v, a)
		width := 0
		for _, f := range fields {
			width = max(width, len(f.name))
		}
		lines := []string{fmt.Sprintf("--%s takes a JSON object, %s:", a.Name, objectShape(fields))}
		for _, f := range fields {
			help := f.help
			if f.required {
				help += " (required)"
			}
			lines = append(lines, fmt.Sprintf("  %-*s  %s", width, f.name, help))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}
	return strings.Join(paras, "\n\n")
}

// helpWidth is the width help text is wrapped to.
const helpWidth = 80

// wrap breaks s into lines of at most width columns at spaces; lines after
// the first are indented by indent spaces.
func wrap(s string, width, indent int) string {
	var b strings.Builder
	col := 0
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
		case col+1+len(word) > width:
			b.WriteString("\n" + strings.Repeat(" ", indent))
			col = indent
		default:
			b.WriteByte(' ')
			col++
		}
		b.WriteString(word)
		col += len(word)
	}
	return b.String()
}

// objectShape writes the fields of an object argument as a JSON skeleton:
// {"owner":"...","summary":"..."}.
func objectShape(fields []subField) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf(`%q:"..."`, f.name)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// orList joins items as "a, b and c" (or "or").
func orList(items []string, conj string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
}

// verbCommand generates the command for one verb: required arguments are
// positional in table order, the others are long flags of the same name.
func (a *app) verbCommand(v api.Verb) *cobra.Command {
	req, opt := splitArgs(v)
	use := v.Name
	for _, arg := range req {
		use += " " + placeholder(arg)
	}
	exs := append([]string{example(v)}, extraExamples[v.Name]...)
	cmd := &cobra.Command{
		Use:     use,
		Short:   v.Summary,
		Long:    longHelp(v),
		Example: "  " + strings.Join(exs, "\n  "),
		Args:    positional(req),
		Annotations: map[string]string{
			annUsage:   usageLine(v),
			annExample: example(v),
			annVerb:    v.Name,
		},
		RunE: func(cmd *cobra.Command, args []string) error { return a.runVerb(cmd, v, args) },
	}
	fs := cmd.Flags()
	fs.SortFlags = false
	for _, arg := range opt {
		switch arg.Kind {
		case api.KindBool:
			fs.Bool(arg.Name, false, arg.Help)
		case api.KindStrings:
			fs.StringSlice(arg.Name, nil, arg.Help+" (repeat the flag or separate with commas)")
		case api.KindObject:
			fs.String(arg.Name, "", arg.Help+", as a JSON object")
		default:
			help := arg.Help
			if len(arg.Enum) > 0 {
				help += ": " + orList(arg.Enum, "or")
			}
			fs.String(arg.Name, "", help)
		}
	}
	fs.String("worktree", "", "act on the worktree containing this directory (default: the current directory)")
	fs.Bool("json", false, "print the structured result as JSON")
	return cmd
}

// positional checks the positional arguments: exactly the required ones,
// non-empty, and within their enum.
func positional(req []api.Arg) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) < len(req):
			missing := make([]string, 0, len(req)-len(args))
			for _, a := range req[len(args):] {
				missing = append(missing, placeholder(a))
			}
			return usagef(cmd, "missing %s", orList(missing, "and"))
		case len(args) > len(req) && len(req) == 0:
			return usagef(cmd, "takes no arguments, got %s", quoteAll(args))
		case len(args) > len(req):
			return usagef(cmd, "expected %d argument%s (%s), got %d; quote arguments that contain spaces",
				len(req), plural(len(req)), joinPlaceholders(req), len(args))
		}
		for i, a := range req {
			if strings.TrimSpace(args[i]) == "" {
				return usagef(cmd, "%s must not be empty", placeholder(a))
			}
			if len(a.Enum) > 0 && !slices.Contains(a.Enum, args[i]) {
				return usagef(cmd, "%s must be %s, got %q", placeholder(a), orList(a.Enum, "or"), args[i])
			}
		}
		return nil
	}
}

func quoteAll(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = strconv.Quote(a)
	}
	return strings.Join(q, " ")
}

func joinPlaceholders(args []api.Arg) string {
	ps := make([]string, len(args))
	for i, a := range args {
		ps[i] = placeholder(a)
	}
	return strings.Join(ps, " ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runVerb maps the command line onto the verb's input, runs it on an engine
// for the worktree and prints the result.
func (a *app) runVerb(cmd *cobra.Command, v api.Verb, args []string) error {
	fs := cmd.Flags()
	dir, err := worktreeDir(fs)
	if err != nil {
		return failed(err)
	}
	in, err := buildInput(cmd, v, args, dir)
	if err != nil {
		return err
	}
	agent, err := agentOverride()
	if err != nil {
		return failed(err)
	}
	out, err := a.call(cmd.Context(), engine.Options{Dir: dir, Client: "cli", Agent: agent}, v.Name, in)
	if err != nil {
		return failed(err)
	}
	asJSON, _ := fs.GetBool("json")
	if err := a.printResult(out, asJSON); err != nil {
		return failed(err)
	}
	if out.Envelope().Outcome == api.Refused {
		return &exitErr{code: exitRefused}
	}
	return nil
}

// worktreeDir is the --worktree flag as an absolute path, or the current
// directory.
func worktreeDir(fs *pflag.FlagSet) (string, error) {
	dir, _ := fs.GetString("worktree")
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cli: current directory: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cli: --worktree %s: %w", dir, err)
	}
	return abs, nil
}

// agentOverride reads GITLET_AGENT.
func agentOverride() (model.AgentID, error) {
	id := model.AgentID(os.Getenv("GITLET_AGENT"))
	if id != "" && !model.ValidAgentID(id) {
		return "", fmt.Errorf("cli: GITLET_AGENT=%q is not a valid agent id: use lowercase letters, digits and dashes, at most 48", id)
	}
	return id, nil
}

// buildInput turns positional arguments and the flags that were set into
// the verb's input type. Only flags given on the command line are included,
// so the rest keep their zero values.
func buildInput(cmd *cobra.Command, v api.Verb, args []string, dir string) (any, error) {
	fs := cmd.Flags()
	m := map[string]any{}
	if fs.Changed("worktree") {
		m["worktree"] = dir
	}
	i := 0
	for _, arg := range v.Args {
		if arg.Required {
			m[arg.Name] = args[i]
			i++
			continue
		}
		switch arg.Kind {
		case api.KindBool:
			if fs.Changed(arg.Name) {
				m[arg.Name], _ = fs.GetBool(arg.Name)
			}
		case api.KindStrings:
			if fs.Changed(arg.Name) {
				vals, _ := fs.GetStringSlice(arg.Name)
				m[arg.Name] = nonEmpty(vals)
			}
		case api.KindObject:
			if fs.Changed(arg.Name) {
				obj, err := objectArg(cmd, v, arg)
				if err != nil {
					return nil, err
				}
				m[arg.Name] = obj
			}
		default:
			if fs.Changed(arg.Name) {
				val, _ := fs.GetString(arg.Name)
				if len(arg.Enum) > 0 && !slices.Contains(arg.Enum, val) {
					return nil, usagef(cmd, "--%s must be %s, got %q", arg.Name, orList(arg.Enum, "or"), val)
				}
				m[arg.Name] = val
			}
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, failed(fmt.Errorf("cli: %s: encode arguments: %w", v.Name, err))
	}
	in := v.NewInput()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		return nil, failed(fmt.Errorf("cli: %s: arguments do not fit the input type: %w", v.Name, err))
	}
	return in, nil
}

// objectArg checks the JSON object given for an object argument: valid
// JSON, only known fields, required fields set, and values of the right
// types. It returns the object as given.
func objectArg(cmd *cobra.Command, v api.Verb, arg api.Arg) (json.RawMessage, error) {
	raw, _ := cmd.Flags().GetString(arg.Name)
	fields := objectFields(v, arg)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
		return nil, usagef(cmd, "--%s must be a JSON object such as '%s'", arg.Name, objectShape(fields))
	}
	var unknown, missing []string
	for name := range obj {
		if !slices.ContainsFunc(fields, func(f subField) bool { return f.name == name }) {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	for _, f := range fields {
		var s string
		if val, ok := obj[f.name]; f.required && (!ok || json.Unmarshal(val, &s) == nil && strings.TrimSpace(s) == "") {
			missing = append(missing, f.name)
		}
	}
	switch {
	case len(unknown) > 0:
		slices.Sort(unknown)
		return nil, usagef(cmd, "--%s has unknown field%s %s; its fields are %s", arg.Name, plural(len(unknown)),
			strings.Join(unknown, ", "), objectShape(fields))
	case len(missing) > 0:
		return nil, usagef(cmd, "--%s needs %s", arg.Name, orList(missing, "and"))
	}
	if f, ok := fieldByJSONName(v.Input, arg.Name); ok {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(reflect.New(f.Type).Interface()); err != nil {
			var te *json.UnmarshalTypeError
			if errors.As(err, &te) {
				return nil, usagef(cmd, "--%s: %s must be a JSON %s, got %s", arg.Name, te.Field, jsonKind(te.Type), te.Value)
			}
			return nil, usagef(cmd, "--%s: %v", arg.Name, err)
		}
	}
	return json.RawMessage(raw), nil
}

// jsonKind names the JSON type a Go type decodes from.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct, reflect.Pointer:
		return "object"
	}
	return "number"
}

func nonEmpty(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, s := range vals {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// call runs one verb on a fresh engine and closes it. The CLI never holds
// the session lock and does no background work; the verb itself refreshes
// stale state.
func (a *app) call(ctx context.Context, opt engine.Options, verb string, input any) (api.Output, error) {
	e, err := a.deps.openEngine(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("cli: %s: %w", verb, err)
	}
	out, err := e.Call(ctx, verb, input)
	if cerr := e.Close(); cerr != nil {
		fmt.Fprintf(a.stderr, "gitlet: warning: closing the engine: %v\n", cerr)
	}
	if err != nil {
		return nil, fmt.Errorf("cli: %s: %w", verb, err)
	}
	if out == nil || isNilPointer(out) {
		return nil, fmt.Errorf("cli: %s: the engine returned no result", verb)
	}
	return out, nil
}

func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// printResult writes the text block, or the reply as indented JSON.
func (a *app) printResult(out api.Output, asJSON bool) error {
	if asJSON {
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("cli: encode result: %w", err)
		}
		_, err = fmt.Fprintf(a.stdout, "%s\n", data)
		return err
	}
	text := render.Text(out.Envelope(), render.CLI)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := fmt.Fprint(a.stdout, text)
	return err
}
