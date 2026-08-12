package mcpbridge_test

import (
	"context"
	"testing"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
)

// The manifests below go through internal/apps' real parser rather than being
// hand-built structs. A test that constructs a Manifest by hand can construct
// one the parser would have refused, and then asserts something about an app
// that could never be installed.

const standupManifest = `{
  "id": "dev.alexis.standup-notes",
  "name": "Standup Notes",
  "version": "1.0.0",
  "description": "Turns the standup you just had into notes and commitments.",
  "author": { "name": "Alexis Massicotte" },
  "permissions": [
    { "scope": "memory.read", "reason": "To read the transcript of the meeting you just left." },
    { "scope": "memory.write", "reason": "To save the notes and commitments it extracts." },
    { "scope": "agent.session", "reason": "To summarise the meeting using your own agent." },
    { "scope": "glasses.speaker", "reason": "To read the commitments back to you when you ask." }
  ],
  "triggers": [
    { "type": "phrase", "match": "wrap up the standup" },
    { "type": "tool", "description": "Summarise the most recent meeting into decisions and commitments." }
  ],
  "timeoutMs": 60000
}`

// An app with no tool trigger: invokable by hand, never by the agent.
const quietManifest = `{
  "id": "dev.alexis.photo-log",
  "name": "Photo Log",
  "version": "1.0.0",
  "description": "Files the stills you capture into the day.",
  "author": { "name": "Alexis Massicotte" },
  "permissions": [
    { "scope": "memory.read", "reason": "To find the episode a still belongs to." }
  ],
  "triggers": [ { "type": "touch", "gesture": "doubleTap" } ]
}`

// An app that can leave the machine.
const filerManifest = `{
  "id": "dev.alexis.issue-filer",
  "name": "Issue Filer",
  "version": "2.1.0",
  "description": "Files the commitments you made as issues on your tracker.",
  "author": { "name": "Alexis Massicotte" },
  "permissions": [
    { "scope": "memory.read", "reason": "To read the commitments it is about to file." },
    { "scope": "net.fetch", "reason": "To create the issues on your tracker." }
  ],
  "allowedHosts": ["api.linear.app"],
  "triggers": [ { "type": "tool", "description": "File your open commitments as issues." } ]
}`

// A read-only app, so the two halves of the grant can be told apart.
const readerManifest = `{
  "id": "dev.alexis.day-reader",
  "name": "Day Reader",
  "version": "1.0.0",
  "description": "Answers questions about what happened today.",
  "author": { "name": "Alexis Massicotte" },
  "permissions": [
    { "scope": "memory.read", "reason": "To read the episodes it is asked about." }
  ],
  "triggers": [ { "type": "tool", "description": "Answer a question about what happened today." } ]
}`

// install parses a manifest and grants everything it asked for, which is the
// ordinary case: the user accepted the sheet.
func install(t *testing.T, manifest string) apps.Installed {
	t.Helper()
	return installGranting(t, manifest, nil)
}

// installGranting is the user declining. nil means "granted everything".
func installGranting(t *testing.T, manifest string, granted []apps.Scope) apps.Installed {
	t.Helper()
	m, err := apps.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatalf("this fixture manifest does not parse: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("this fixture manifest does not validate: %v", err)
	}
	if granted == nil {
		granted = m.Scopes()
	}
	return apps.Installed{Manifest: m, Granted: granted}
}

// catalogue is the installed list a test hands the bridge.
type catalogue struct{ list []apps.Installed }

func (c *catalogue) Apps(context.Context) []apps.Installed { return c.list }

func (c *catalogue) remove(id string) {
	var kept []apps.Installed
	for _, a := range c.list {
		if a.Manifest.ID != id {
			kept = append(kept, a)
		}
	}
	c.list = kept
}

// recorder is an [mcpbridge.Invoker] that remembers what it was asked to run.
type recorder struct {
	calls    []apps.TriggerFrame
	ran      []string
	outcome  mcpbridge.Outcome
	failWith error
}

func (r *recorder) InvokeApp(_ context.Context, a apps.Installed, t apps.TriggerFrame) (mcpbridge.Outcome, error) {
	r.calls = append(r.calls, t)
	r.ran = append(r.ran, a.Manifest.ID)
	if r.failWith != nil {
		return mcpbridge.Outcome{}, r.failWith
	}
	return r.outcome, nil
}
