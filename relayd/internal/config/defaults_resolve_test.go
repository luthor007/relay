package config

import (
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// Every id in Default() has to name something that exists.
//
// This exists because one of them did not. Default() wrote
// Voice.Fallback: "edge-tts" against a catalog whose id is "edge", so `relay
// doctor` on a config nobody had touched reported the keyless voice as "no such
// voice option" — the row whose entire purpose is that a user who skips the
// voice step still has a device that talks.
//
// Nothing caught it because a default is a string and a catalog is a list, and
// no test had ever asked whether the string was in the list. Each of these
// assertions is one string, and together they are the reason that cannot happen
// again quietly.

func TestDefaultVoiceIdsResolveAgainstTheCatalog(t *testing.T) {
	d := Default()

	primary, ok := voice.Get(d.Voice.Provider)
	if !ok {
		t.Fatalf("Voice.Provider = %q, which is not in the voice catalog", d.Voice.Provider)
	}
	if !primary.Recommended {
		t.Errorf("Voice.Provider = %q, which is not the recommended row; "+
			"ORCHESTRATOR.md §2a makes the voice step a recommendation, not a coin flip",
			d.Voice.Provider)
	}

	fallback, ok := voice.Get(d.Voice.Fallback)
	if !ok {
		t.Fatalf("Voice.Fallback = %q, which is not in the voice catalog — "+
			"a fallback that resolves to nothing is a mute device", d.Voice.Fallback)
	}
	if !fallback.Keyless {
		t.Errorf("Voice.Fallback = %q, which needs a credential; the fallback has "+
			"to work for someone who skipped the step entirely", d.Voice.Fallback)
	}
}

func TestDefaultVoicePlanCannotGoMute(t *testing.T) {
	d := Default()
	plan := voice.Plan{Primary: d.Voice.Provider, Fallback: d.Voice.Fallback}
	if err := plan.Validate(); err != nil {
		t.Fatalf("the default voice plan does not validate: %v", err)
	}
}

func TestDefaultModelVendorsResolve(t *testing.T) {
	d := Default()
	for name, m := range map[string]Model{"small": d.Models.Small, "big": d.Models.Big} {
		if m.Vendor == "" {
			t.Errorf("Models.%s has no vendor", name)
			continue
		}
		if _, ok := llm.Vendor(m.Vendor); !ok {
			t.Errorf("Models.%s.Vendor = %q, which is not in the vendor catalog",
				name, m.Vendor)
		}
		if m.Model == "" {
			t.Errorf("Models.%s has no model id", name)
		}
	}
}

// The embedding width is welded shut once the index exists: a vec0 column's
// dimension is fixed at create time, so a default that disagrees with the
// store's constant is not a preference, it is a schema error waiting for the
// first search after a two-hour backfill.
func TestDefaultEmbeddingWidthMatchesTheIndex(t *testing.T) {
	d := Default()
	if d.Embedding.Dims != EmbeddingDims {
		t.Errorf("Embedding.Dims = %d, want %d", d.Embedding.Dims, EmbeddingDims)
	}
	if d.Embedding.Model == "" {
		t.Error("Embedding.Model is empty; the installer cannot provision nothing")
	}
	if d.Embedding.Provider == "" {
		t.Error("Embedding.Provider is empty")
	}
}
