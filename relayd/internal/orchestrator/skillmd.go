package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillsDirName is where exported playbooks land under the data directory.
const SkillsDirName = "skills"

// ExportSkillMD writes a skill in the portable on-disk format.
//
// The MCP bus reaches every runtime pointed at our gateway, which is the strong
// path — it is live, it is grant-gated, and a mid-session change can be pushed.
// But it reaches nothing that is *not* pointed at us, and the ecosystem has
// settled on a file format that everything reads: OpenClaw ships 52 skills as
// `<name>/SKILL.md`, Hermes keeps its own in `~/.hermes/skills/`, and Anthropic
// Agent Skills use the same shape. `name` and `description` are the portable
// core; `metadata.<runtime>` is the per-runtime namespace.
//
// So this is the second, weaker, wider distribution: a directory a human can
// read, copy, check into a repository, or point another agent at. Writing both
// is what makes "share tools with the other frameworks" true rather than true
// of the frameworks that happen to be on our bus.
func ExportSkillMD(root string, s Skill) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	dir := filepath.Join(root, s.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(SkillMD(s)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// SkillMD renders the file.
//
// The description is the trigger sentence rather than a summary of the steps,
// because description is the field every runtime shows to its model when
// deciding whether to load the skill — it is a "when", not a "what", and
// writing it as a "what" is the one mistake that makes a skill never fire.
func SkillMD(s Skill) string {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", s.Name)
	fmt.Fprintf(&b, "description: %s\n", yamlString(skillDescription(s)))
	b.WriteString("metadata:\n")
	b.WriteString("  relay:\n")
	fmt.Fprintf(&b, "    origin: %s\n", s.Origin)
	if !s.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "    created: %s\n", s.CreatedAt.UTC().Format("2006-01-02"))
	}
	if len(s.Needs) > 0 {
		fmt.Fprintf(&b, "    needs: [%s]\n", strings.Join(s.Needs, ", "))
	}
	b.WriteString("---\n\n")

	title := s.Title
	if title == "" {
		title = s.Name
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "Use this %s.\n\n", strings.TrimSuffix(strings.TrimSpace(s.When), "."))
	b.WriteString(strings.TrimSpace(s.Steps))
	b.WriteString("\n")

	if len(s.Needs) > 0 {
		fmt.Fprintf(&b, "\nThis needs: %s. If you do not have one of these, say so rather than improvising.\n",
			strings.Join(s.Needs, ", "))
	}

	// Said in the file as well as in the tool description, because a file gets
	// copied out of here into places where nothing else explains it.
	b.WriteString("\n---\n")
	b.WriteString("_Written by Relay. These are instructions for you to carry out — " +
		"Relay orchestrates and does not execute._\n")
	return b.String()
}

// yamlString quotes a scalar when it has to be. A description containing a
// colon is the common case ("Call this when: ...") and unquoted YAML would
// silently parse it as a nested mapping.
func yamlString(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if !strings.ContainsAny(s, ":#\"'{}[]|>&*!%@`,") {
		return s
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
