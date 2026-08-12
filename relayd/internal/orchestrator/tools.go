// Package orchestrator is ORCHESTRATOR.md §3b wired end to end: the small model
// hears every utterance and speaks, the big model holds the tools and does the
// work.
//
// Both halves already existed and neither could reach the other.
// [routing.Classify] is the escalation allowlist, [routing.Narration] is the
// grounded voice, and [llm.Loop] is the agentic loop — this package is the
// composition, which is where the design decisions that neither half could make
// alone actually live:
//
//   - The two models never share a conversation. The big one has messages; the
//     small one has events and has no way to be handed a transcript. That is
//     partly a lie-prevention property (§3b's narration drift) and partly a
//     cost one: switching models inside one conversation invalidates the prompt
//     cache on every provider that has one, so the cheap path would stop being
//     cheap the moment it was wired the obvious way.
//   - The acknowledgement is spoken before the loop starts, not after the first
//     tool returns. SYSTEM.md §7b's budget is ~400 ms to first word against an
//     agent that takes 1–10 s, so anything that waits for the work has already
//     lost.
//   - The big model's tool set is five tools. Anthropic's own tool guidance
//     names a bloated set with ambiguous decision points as the most common
//     failure, with the test being whether a human engineer could say which
//     tool applies; five is what survives that test here.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// Session is one row the big model can see.
type Session struct {
	ID        string
	Runtime   string
	Subject   string
	Workspace string
	State     string
	Since     time.Time
}

// Hit is one memory result.
type Hit struct {
	SessionID string
	Title     string
	Snippet   string
	When      time.Time
}

// Sessions is what the big model can do to agent sessions. Deliberately four
// verbs: the orchestrator drives runtimes, it does not become one.
type Sessions interface {
	List(ctx context.Context) ([]Session, error)
	Start(ctx context.Context, runtime, workspace, prompt string) (Session, error)
	Send(ctx context.Context, id, text string) error
	Stop(ctx context.Context, id string) error
}

// Memory is the read half of MEMORY.md's index.
type Memory interface {
	Search(ctx context.Context, query string, limit int) ([]Hit, error)
}

// Consequential names the tools ORCHESTRATOR.md §4b requires a human to confirm
// every time.
//
// The criterion is reversibility, not danger. Listing sessions and searching
// memory change nothing and are free to get wrong; starting an agent session
// spends money and touches a repository, and stopping one throws away work in
// progress. Sending a turn to a session the user is already talking to is the
// deliberate exception — it is the normal way work happens, and confirming
// every sentence would make the device useless.
func Consequential(tool string) bool {
	switch tool {
	case ToolStartSession, ToolStopSession, ToolGrantCapability, ToolAuthorSkill:
		return true
	default:
		return false
	}
}

// The tool names, as constants so the policy and the tests cannot drift from
// the declarations.
const (
	ToolListSessions     = "list_sessions"
	ToolSearchMemory     = "search_memory"
	ToolStartSession     = "start_session"
	ToolSendToSession    = "send_to_session"
	ToolStopSession      = "stop_session"
	ToolListCapabilities = "list_capabilities"
	ToolGrantCapability  = "grant_capability"
	ToolRemember         = "remember"
	ToolAuthorSkill      = "author_skill"
	ToolDescribeRuntime  = "describe_runtime"
)

// kinds classifies every tool. [Toolbox] refuses to build one that is missing,
// which is how the no-executor rule in [Kind] stays true as tools are added.
var kinds = map[string]Kind{
	ToolListSessions:     KindRead,
	ToolSearchMemory:     KindRead,
	ToolListCapabilities: KindRead,
	ToolDescribeRuntime:  KindRead,
	ToolSendToSession:    KindInstruct,
	ToolStartSession:     KindInstruct,
	ToolStopSession:      KindInstruct,
	ToolGrantCapability:  KindProvision,
	ToolRemember:         KindProvision,
	ToolAuthorSkill:      KindProvision,
}

// KindOf reports what a tool does to the world.
func KindOf(tool string) (Kind, bool) {
	k, ok := kinds[tool]
	return k, ok
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	// Structured outputs require this on every object, and it is the right
	// default anyway: a model that invents a sixth field is telling us the
	// schema is wrong.
	s["additionalProperties"] = false
	return s
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// Deps is everything the big model can be given. Every field is optional, and
// a nil one simply removes its tools: an install with no connectors still
// routes sessions, and an install with no memory still starts work.
type Deps struct {
	Sessions     Sessions
	Memory       Memory
	Notebook     Notebook
	Capabilities Capabilities
	Skills       Skills
}

// Toolbox builds the big model's tools.
//
// Every description says when to call the tool and not only what it does. That
// is not style: recent Opus models reach for tools conservatively, and the
// trigger clause is what moves the should-call rate. It is also where the
// judgement lives that would otherwise be a paragraph of system prompt nobody
// can test.
func Toolbox(s Sessions, m Memory) llm.Toolbox {
	return ToolboxFor(Deps{Sessions: s, Memory: m})
}

// ToolboxFor builds the tools for a given set of dependencies.
func ToolboxFor(d Deps) llm.Toolbox {
	box := coreToolbox(d)
	box = append(box, provisionToolbox(d)...)

	// The no-executor rule, enforced rather than documented: a tool nobody
	// classified cannot ship. See [Kind].
	for _, b := range box {
		if _, ok := KindOf(b.Tool.Name); !ok {
			panic("orchestrator: tool " + b.Tool.Name + " has no Kind; see the no-executor rule in kind.go")
		}
	}
	return box
}

func coreToolbox(d Deps) llm.Toolbox {
	s, m := d.Sessions, d.Memory

	// Always present: it depends on nothing, and it is the tool that makes the
	// other four worth having. Choosing a runtime without it is choosing on the
	// basis of the name.
	box := llm.Toolbox{{
		Tool: llm.Tool{
			Name: ToolDescribeRuntime,
			Description: "Get the brief for an agent runtime: what it is good at, how to prompt it, its " +
				"commands, what it cannot do, and the traps. Call this before sending work to a " +
				"runtime you have not used in this conversation, and whenever you are choosing " +
				"between two of them.",
			Schema: object(map[string]any{
				"runtime": str("One of claude-code, codex, openclaw, hermes, opencode. " +
					"Leave empty for a one-line summary of all five."),
			}),
			ParallelSafe: true,
		},
		Run: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			name := strings.TrimSpace(c.Arg("runtime"))
			if name == "" {
				return llm.ToolResult{Content: Roster()}, nil
			}
			h, ok := HarnessFor(adapter.Runtime(name))
			if !ok {
				// Naming the five is more useful than "unknown": the model
				// guessed a name and needs the real ones, not a refusal.
				return llm.ToolResult{
					Content: fmt.Sprintf("No runtime called %q. These are the five:\n\n%s", name, Roster()),
					IsError: true,
				}, nil
			}
			return llm.ToolResult{Content: h.Brief()}, nil
		},
	}}

	if s != nil {
		box = append(box,
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolListSessions,
					Description: "List the agent sessions running right now, with what each is working on. " +
						"Call this before starting anything new, and whenever the user refers to work " +
						"as if it already exists (\"the payments one\", \"that refactor\").",
					Schema:       object(nil),
					ParallelSafe: true,
				},
				Run: func(ctx context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
					list, err := s.List(ctx)
					if err != nil {
						return llm.ToolResult{}, err
					}
					if len(list) == 0 {
						// "Nothing found" always comes with where we looked;
						// an empty string reads as a broken tool.
						return llm.ToolResult{Content: "No sessions are running."}, nil
					}
					var b strings.Builder
					for _, x := range list {
						fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n",
							x.ID, x.Runtime, x.State, x.Subject, x.Workspace)
					}
					return llm.ToolResult{Content: b.String()}, nil
				},
			},
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolStartSession,
					Description: "Start a new agent session on a workspace. Call this only when the user " +
						"asks for work that list_sessions shows is not already running. " +
						"This spends money and touches a repository, so it is confirmed with the user first.",
					Schema: object(map[string]any{
						"prompt":    str("What the agent should do, in the user's own words where possible."),
						"workspace": str("Absolute path to the repository or directory."),
						"runtime":   str("Which runtime to use. Leave empty to let relay choose."),
					}, "prompt"),
				},
				Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
					var in struct {
						Prompt    string `json:"prompt"`
						Workspace string `json:"workspace"`
						Runtime   string `json:"runtime"`
					}
					if err := json.Unmarshal(c.Input, &in); err != nil {
						return llm.ToolResult{}, err
					}
					if strings.TrimSpace(in.Prompt) == "" {
						return llm.ToolResult{Content: "a session needs a prompt", IsError: true}, nil
					}
					sess, err := s.Start(ctx, in.Runtime, in.Workspace, in.Prompt)
					if err != nil {
						return llm.ToolResult{}, err
					}
					return llm.ToolResult{Content: fmt.Sprintf(
						"started session %s on %s", sess.ID, sess.Runtime)}, nil
				},
			},
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolSendToSession,
					Description: "Send a message to a session that is already running. Call this when the " +
						"user is adding to, correcting, or answering work in progress rather than " +
						"asking for something new.",
					Schema: object(map[string]any{
						"session": str("The session id from list_sessions."),
						"text":    str("What to say to it."),
					}, "session", "text"),
				},
				Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
					var in struct{ Session, Text string }
					if err := json.Unmarshal(c.Input, &in); err != nil {
						return llm.ToolResult{}, err
					}
					if err := s.Send(ctx, in.Session, in.Text); err != nil {
						return llm.ToolResult{}, err
					}
					return llm.ToolResult{Content: "delivered to " + in.Session}, nil
				},
			},
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolStopSession,
					Description: "Stop a running session. Call this when the user says to stop, cancel or " +
						"abandon work. Anything the session had not finished is lost, so it is " +
						"confirmed with the user first.",
					Schema: object(map[string]any{
						"session": str("The session id from list_sessions."),
					}, "session"),
				},
				Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
					var in struct{ Session string }
					if err := json.Unmarshal(c.Input, &in); err != nil {
						return llm.ToolResult{}, err
					}
					if err := s.Stop(ctx, in.Session); err != nil {
						return llm.ToolResult{}, err
					}
					return llm.ToolResult{Content: "stopped " + in.Session}, nil
				},
			},
		)
	}

	if m != nil {
		box = append(box, llm.Binding{
			Tool: llm.Tool{
				Name: ToolSearchMemory,
				Description: "Search past sessions across every runtime on this machine. Call this when " +
					"the user refers to something already decided, tried or discussed — " +
					"\"what did we settle on\", \"have I hit this before\" — rather than guessing.",
				Schema: object(map[string]any{
					"query": str("What to look for, in plain words."),
					"limit": map[string]any{"type": "integer", "description": "How many results. Default 5."},
				}, "query"),
				ParallelSafe: true,
			},
			Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
				var in struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return llm.ToolResult{}, err
				}
				if in.Limit <= 0 || in.Limit > 25 {
					in.Limit = 5
				}
				hits, err := m.Search(ctx, in.Query, in.Limit)
				if err != nil {
					return llm.ToolResult{}, err
				}
				if len(hits) == 0 {
					return llm.ToolResult{Content: "Nothing in the index matches that."}, nil
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "%s\t%s\t%s\n\t%s\n",
						h.SessionID, h.When.Format(time.DateOnly), h.Title, h.Snippet)
				}
				return llm.ToolResult{Content: b.String()}, nil
			},
		})
	}
	return box
}

// provisionToolbox is everything that prepares work rather than doing it: what
// an agent is allowed to reach, what is worth remembering, and what Relay has
// learned to ask for.
func provisionToolbox(d Deps) llm.Toolbox {
	var box llm.Toolbox

	if d.Capabilities != nil {
		box = append(box,
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolListCapabilities,
					Description: "List what agents on this machine can be given access to — a browser, " +
						"dashboards, mail, issue trackers — and which are already granted. " +
						"Call this before telling a session to do something that needs one, so you " +
						"can say what is missing instead of watching it fail.",
					Schema:       object(nil),
					ParallelSafe: true,
				},
				Run: func(ctx context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
					caps, err := d.Capabilities.List(ctx)
					if err != nil {
						return llm.ToolResult{}, err
					}
					if len(caps) == 0 {
						return llm.ToolResult{Content: "Nothing is connected on this machine yet."}, nil
					}
					var b strings.Builder
					for _, c := range caps {
						state := "not granted"
						if c.Granted {
							state = "granted"
						}
						fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", c.Name, c.Half, state, c.Summary)
					}
					return llm.ToolResult{Content: b.String()}, nil
				},
			},
			llm.Binding{
				Tool: llm.Tool{
					Name: ToolGrantCapability,
					Description: "Ask the user to grant a capability. Call this when a task needs one " +
						"list_capabilities showed as not granted. Granting once makes it work in " +
						"every agent runtime on the machine, and revoking once withdraws it from " +
						"all of them. The user decides; you are asking.",
					Schema: object(map[string]any{
						"name": str("The capability name from list_capabilities."),
						"half": map[string]any{
							"type": "string", "enum": []any{"read", "write"},
							"description": "Read is looking; write is acting. Ask for the smaller one that does the job.",
						},
					}, "name", "half"),
				},
				Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
					var in struct{ Name, Half string }
					if err := json.Unmarshal(c.Input, &in); err != nil {
						return llm.ToolResult{}, err
					}
					// "glasses" is the surface, and it goes in the audit line
					// because "granted at the glasses" and "granted in the
					// console" are different stories about the same row.
					if err := d.Capabilities.Grant(ctx, in.Name, in.Half, "glasses"); err != nil {
						return llm.ToolResult{}, err
					}
					return llm.ToolResult{Content: fmt.Sprintf(
						"%s:%s is granted, in every runtime on this machine", in.Name, in.Half)}, nil
				},
			},
		)
	}

	if d.Notebook != nil {
		box = append(box, llm.Binding{
			Tool: llm.Tool{
				Name: ToolRemember,
				Description: "Write down something worth knowing next time — a decision, a preference, " +
					"a piece of how this machine is set up. Call this when the user tells you " +
					"something that will still be true in a month, not to summarise what just " +
					"happened; the session index already holds that.",
				Schema: object(map[string]any{
					"text":    str("The fact, in a sentence that will still make sense out of context."),
					"subject": str("What it is about — a repository, a service, a person."),
				}, "text"),
			},
			Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
				var in struct{ Text, Subject string }
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return llm.ToolResult{}, err
				}
				if strings.TrimSpace(in.Text) == "" {
					return llm.ToolResult{Content: "a fact needs text", IsError: true}, nil
				}
				if err := d.Notebook.Remember(ctx, Fact{
					Text: in.Text, Subject: in.Subject,
					// The turn this was said in, so the console can show the
					// sentence next to the fact a month later.
					Session: SessionFrom(ctx),
					At:      time.Now(),
				}); err != nil {
					return llm.ToolResult{}, err
				}
				return llm.ToolResult{Content: "remembered"}, nil
			},
		})
	}

	if d.Skills != nil {
		box = append(box, llm.Binding{
			Tool: llm.Tool{
				Name: ToolAuthorSkill,
				Description: "Write a playbook that every agent runtime on this machine can then use. " +
					"Call this after working out a sequence worth repeating — how to check a " +
					"dashboard, how this project is deployed. Write instructions for an agent to " +
					"carry out, not code: the playbook is read by whichever agent has the browser " +
					"or the shell, and it is that agent that acts.",
				Schema: object(map[string]any{
					"name":  str("Short lowercase name with underscores, e.g. check_staging_health."),
					"title": str("One line a person would recognise in a list."),
					"when":  str("The trigger: \"when the user asks whether staging is healthy\"."),
					"steps": str("What the executing agent should do, in order, in plain imperative sentences."),
					"needs": map[string]any{
						"type": "array", "items": map[string]any{"type": "string"},
						"description": "Capabilities the agent must already have, e.g. browser, kubectl.",
					},
				}, "name", "when", "steps"),
			},
			Run: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
				var in struct {
					Name  string   `json:"name"`
					Title string   `json:"title"`
					When  string   `json:"when"`
					Steps string   `json:"steps"`
					Needs []string `json:"needs"`
				}
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return llm.ToolResult{}, err
				}
				skill := Skill{
					Name: in.Name, Title: in.Title, When: in.When,
					Steps: in.Steps, Needs: in.Needs, Origin: "orchestrator",
				}
				if err := skill.Validate(); err != nil {
					// A validation failure is something the model can fix on
					// the next turn, so it is an error result rather than a
					// failed run.
					return llm.ToolResult{Content: err.Error(), IsError: true}, nil
				}
				if err := d.Skills.Author(ctx, skill); err != nil {
					return llm.ToolResult{}, err
				}
				return llm.ToolResult{Content: fmt.Sprintf(
					"%s is now available to every runtime on this machine", skill.Name)}, nil
			},
		})
	}
	return box
}
