package install

import (
	"strings"
	"testing"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/mcpserver"
)

func TestSkillFrontmatter(t *testing.T) {
	s := Skill()
	if !strings.HasPrefix(s, "---\nname: gitlet\ndescription: ") {
		t.Fatalf("SKILL.md must start with frontmatter naming gitlet:\n%.80s", s)
	}
	d := parseSkill(s).description
	if d == "" || strings.Contains(d, ": ") || strings.Contains(d, " #") || strings.ContainsAny(d, "\n\"'") {
		t.Errorf("description must be one plain YAML line: %q", d)
	}
	if n := strings.Count(s, "\n"); n > 120 {
		t.Errorf("SKILL.md has %d lines, keep it under 120", n)
	}
	if !strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\n\n") {
		t.Error("SKILL.md must end in exactly one newline")
	}
}

// TestSkillRules keeps the skill's rules identical, apart from Markdown code
// spans, to the MCP server instructions (docs/SPEC.md, section 13).
func TestSkillRules(t *testing.T) {
	var want []string
	for _, l := range strings.Split(mcpserver.Instructions, "\n") {
		if l != "" && l[0] >= '1' && l[0] <= '9' {
			want = append(want, l)
		}
	}
	var got []string
	for _, l := range parseSkill(Skill()).sections[sectionRules] {
		if l != "" {
			got = append(got, strings.ReplaceAll(l, "`", ""))
		}
	}
	if len(want) != 9 {
		t.Fatalf("found %d rules in mcpserver.Instructions, want 9", len(want))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("SKILL.md rules drifted from mcpserver.Instructions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSkillVerbs(t *testing.T) {
	verbs := strings.Join(parseSkill(Skill()).sections["Verbs"], "\n")
	for _, v := range api.Verbs {
		if !strings.Contains(verbs, "\n- `"+v.Name+"` ") {
			t.Errorf("SKILL.md has no line for verb %s", v.Name)
		}
	}
}

func TestGuidance(t *testing.T) {
	g := guidance()
	d := parseSkill(Skill())
	for _, l := range d.sections[sectionRules] {
		if !strings.Contains(g, l) {
			t.Errorf("short guidance lacks rule %q", l)
		}
	}
	for _, want := range []string{"gitlet coordinates you", "Reading results:\n- `[act]`", `"..."`} {
		if !strings.Contains(g, want) {
			t.Errorf("short guidance lacks %q", want)
		}
	}
	for _, unwanted := range []string{"## ", "start intent=", "message from codex"} {
		if strings.Contains(g, unwanted) {
			t.Errorf("short guidance should not carry %q", unwanted)
		}
	}
	if !strings.HasSuffix(g, "\n") || strings.HasSuffix(g, "\n\n") {
		t.Error("short guidance must end in exactly one newline")
	}

	a := agentsSection()
	if !strings.HasPrefix(a, markerStart+"\n## gitlet\n\n") || !strings.HasSuffix(a, "\n"+markerEnd) || !strings.Contains(a, g) {
		t.Errorf("AGENTS.md section is malformed:\n%s", a)
	}
	c := cursorRule()
	if !strings.HasPrefix(c, "---\ndescription: "+d.description+"\nalwaysApply: true\n---\n\n# gitlet\n\n") || !strings.HasSuffix(c, g) {
		t.Errorf("Cursor rule is malformed:\n%s", c)
	}
}
