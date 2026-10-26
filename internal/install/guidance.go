package install

import (
	_ "embed"
	"strings"
)

// skill is the agent skill installed for Claude Code. It is also the single
// source of the shorter guidance written to AGENTS.md and the Cursor rule,
// which is cut from it so the three never drift.
//
//go:embed skill/SKILL.md
var skill string

// The SKILL.md sections the short guidance takes: all of the rules, and the
// bullets of the section on reading results.
const (
	sectionRules   = "Rules"
	sectionResults = "Reading results"
)

// AGENTS.md markers around gitlet's section.
const (
	markerStart = "<!-- gitlet:start -->"
	markerEnd   = "<!-- gitlet:end -->"
)

// skillDoc is SKILL.md taken apart: the frontmatter description, the text
// between the title and the first section, and each "## " section's lines.
type skillDoc struct {
	description string
	intro       []string
	sections    map[string][]string
}

func parseSkill(s string) skillDoc {
	d := skillDoc{sections: map[string][]string{}}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[0] == "---" {
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				lines = lines[i+1:]
				break
			}
			if v, ok := strings.CutPrefix(lines[i], "description:"); ok {
				d.description = strings.TrimSpace(v)
			}
		}
	}
	section := "" // "#" after the title, then each "## " heading
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "## "):
			section = strings.TrimSpace(l[3:])
		case strings.HasPrefix(l, "# "):
			section = "#"
		case section == "#":
			d.intro = append(d.intro, l)
		case section != "":
			d.sections[section] = append(d.sections[section], l)
		}
	}
	return d
}

// guidance is the short form of the skill, as Markdown without a title: the
// introduction, the rules, and how to read results. It ends in a newline.
func guidance() string {
	d := parseSkill(skill)
	var bullets []string
	for _, l := range d.sections[sectionResults] {
		if strings.HasPrefix(l, "- ") {
			bullets = append(bullets, l)
		}
	}
	var parts []string
	for _, block := range [][]string{d.intro, d.sections[sectionRules]} {
		if s := trimBlankLines(block); s != "" {
			parts = append(parts, s)
		}
	}
	if len(bullets) > 0 {
		parts = append(parts, "Reading results:\n"+strings.Join(bullets, "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// agentsSection is gitlet's section of AGENTS.md, markers included.
func agentsSection() string {
	return markerStart + "\n## gitlet\n\n" + guidance() + markerEnd
}

// cursorRule is .cursor/rules/gitlet.mdc: an always-applied rule.
func cursorRule() string {
	return "---\ndescription: " + parseSkill(skill).description + "\nalwaysApply: true\n---\n\n# gitlet\n\n" + guidance()
}

func trimBlankLines(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
