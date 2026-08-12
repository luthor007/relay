package llm_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// TestTruncateKeepsBothEnds: dropping the tail would drop the half that
// usually matters — a compiler puts its summary at the end, a stack trace puts
// the cause at the top and the site at the bottom.
func TestTruncateKeepsBothEnds(t *testing.T) {
	tool := llm.Tool{Name: "run", MaxResultBytes: 400}
	body := "HEAD-MARKER" + strings.Repeat("x", 4000) + "TAIL-MARKER"

	out, cut := tool.Truncate(body)
	if !cut {
		t.Fatal("a 4 KB result was not truncated at a 400-byte cap")
	}
	if len(out) > 400 {
		t.Errorf("truncated to %d bytes, over the %d cap", len(out), 400)
	}
	for _, want := range []string{"HEAD-MARKER", "TAIL-MARKER", "omitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("truncated output lost %q:\n%s", want, out)
		}
	}
	// The model has to be able to tell truncation from a short result,
	// otherwise it reasons about a complete answer that is not one.
	if !strings.Contains(out, "narrow the call") {
		t.Errorf("the marker does not say what to do about it:\n%s", out)
	}
}

func TestTruncateNeverCutsARuneInHalf(t *testing.T) {
	tool := llm.Tool{Name: "run", MaxResultBytes: 200}
	// Multi-byte throughout, so a byte-slice at any offset lands mid-rune.
	out, cut := tool.Truncate(strings.Repeat("é日本", 500))
	if !cut {
		t.Fatal("not truncated")
	}
	if !utf8.ValidString(out) {
		t.Error("truncation produced invalid UTF-8, which reads to the model as corruption")
	}
}

func TestShortResultsPassThroughUntouched(t *testing.T) {
	tool := llm.Tool{Name: "run"}
	out, cut := tool.Truncate("all 4 tests passed")
	if cut || out != "all 4 tests passed" {
		t.Errorf("out=%q cut=%v", out, cut)
	}
}

func TestValidateToolsCatchesTheSetContradictingItself(t *testing.T) {
	obj := map[string]any{"type": "object"}

	for _, tc := range []struct {
		name  string
		tools []llm.Tool
		want  string
	}{
		{
			"duplicate names",
			[]llm.Tool{
				{Name: "search", Description: "a", Schema: obj},
				{Name: "search", Description: "b", Schema: obj},
			},
			"two tools are named",
		},
		{
			// The one machine-readable form of the failure mode Anthropic's own
			// tool guidance calls the most common: if a human cannot say which
			// tool applies, the model cannot either.
			"identical descriptions",
			[]llm.Tool{
				{Name: "search_memory", Description: "Find things.", Schema: obj},
				{Name: "search_sessions", Description: "find   THINGS.", Schema: obj},
			},
			"same description",
		},
		{
			"no description",
			[]llm.Tool{{Name: "search", Schema: obj}},
			"no description",
		},
		{
			"not an object schema",
			[]llm.Tool{{Name: "search", Description: "a", Schema: map[string]any{"type": "string"}}},
			"object schema",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := llm.ValidateTools(tc.tools)
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestToolboxRejectsADeclarationWithNoHandler(t *testing.T) {
	box := llm.Toolbox{{Tool: llm.Tool{
		Name:        "start_session",
		Description: "Start one.",
		Schema:      map[string]any{"type": "object"},
	}}}
	err := box.Validate()
	if err == nil || !strings.Contains(err.Error(), "no handler") {
		t.Fatalf("err = %v; a tool the model can call and we cannot run is a hang", err)
	}
}
