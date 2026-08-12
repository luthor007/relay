package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/orchestrator"
)

// TestSKILLMDIsThePortableFormat.
//
// The MCP bus reaches every runtime pointed at our gateway and nothing that is
// not. The ecosystem has settled on a file the rest of them read: OpenClaw
// ships 52 skills as <name>/SKILL.md, Hermes keeps its own in ~/.hermes/skills/,
// and Anthropic Agent Skills use the same shape. name + description are the
// portable core; metadata.<runtime> is the per-runtime namespace.
func TestSKILLMDIsThePortableFormat(t *testing.T) {
	root := t.TempDir()
	path, err := orchestrator.ExportSkillMD(root, staging())
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(path)); got != "check_staging_health" {
		t.Errorf("directory = %q; the layout is <name>/SKILL.md", got)
	}
	if filepath.Base(path) != "SKILL.md" {
		t.Errorf("file = %q", filepath.Base(path))
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(body)

	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("no frontmatter:\n%s", md)
	}
	for _, want := range []string{
		"name: check_staging_health",
		"description:",
		"metadata:",
		"  relay:",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("frontmatter is missing %q:\n%s", want, md)
		}
	}

	// The description is the trigger, not a summary of the steps. Every runtime
	// shows this field to its model when deciding whether to load the skill, so
	// writing it as a "what" is the one mistake that makes a skill never fire.
	if !strings.Contains(md, "Call this when the user asks whether staging is up") {
		t.Errorf("the description is not the trigger:\n%s", md)
	}
	// And the body carries the instructions and the honesty about what it is.
	for _, want := range []string{"staging dashboard", "error rate", "does not execute"} {
		if !strings.Contains(md, want) {
			t.Errorf("the body is missing %q:\n%s", want, md)
		}
	}
}

// TestADescriptionWithAColonIsQuoted. "Call this when: ..." is the common shape
// and unquoted YAML parses it as a nested mapping — a skill that silently
// stops loading everywhere.
func TestADescriptionWithAColonIsQuoted(t *testing.T) {
	s := staging()
	s.When = "when the user says: is staging up"
	md := orchestrator.SkillMD(s)

	var line string
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "description:") {
			line = l
		}
	}
	if !strings.Contains(line, `"`) {
		t.Errorf("a description containing a colon was not quoted: %s", line)
	}
}

// TestAuthoringAlsoWritesTheFile — both distributions, from one call, because
// "share tools with the other frameworks" is only true if it reaches the ones
// that are not on our bus.
func TestAuthoringAlsoWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	book := orchestrator.NewSkillBook()
	book.ExportDir = dir

	if err := book.Author(t.Context(), staging()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "check_staging_health", "SKILL.md")); err != nil {
		t.Fatalf("authoring did not export the portable copy: %v", err)
	}
	// And it is still on the bus, which is the path that matters.
	if len(book.Tools(t.Context())) != 1 {
		t.Error("the skill is not on the bus")
	}
}

// TestAFailedExportStillKeepsTheSkill: a read-only directory must not cost the
// user the playbook. The file copy is the wider, weaker distribution.
func TestAFailedExportStillKeepsTheSkill(t *testing.T) {
	book := orchestrator.NewSkillBook()
	book.ExportDir = filepath.Join(t.TempDir(), "nope", "\x00bad")

	err := book.Author(t.Context(), staging())
	if err == nil {
		t.Fatal("a failed export was silent")
	}
	var ex *orchestrator.ErrExport
	if !errorsAs(err, &ex) {
		t.Fatalf("err = %T %v; the caller cannot tell 'you have the skill' from 'you do not'", err, err)
	}
	if len(book.Tools(t.Context())) != 1 {
		t.Error("a failed export lost the skill")
	}
}

func errorsAs(err error, target **orchestrator.ErrExport) bool {
	for err != nil {
		if e, ok := err.(*orchestrator.ErrExport); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
