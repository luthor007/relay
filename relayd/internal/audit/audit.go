// Package audit is the append-only record of every credential and connector
// mutation: who, what, when, from where.
//
// DASHBOARD.md §4 puts this in the "both deployments" column, next to the two
// auth models rather than under them: *every credential and connector mutation
// is written to an audit log the console itself displays. If something reads
// keys it should not, the evidence exists in a place the user can see without
// our help.*
//
// Three properties follow from that sentence, and each is enforced here rather
// than left to the caller's discipline:
//
//   - **The attempt is recorded, not only the success.** A log that only holds
//     what worked cannot answer "did anything try". [Begin] writes the attempt
//     before the mutation runs and returns a handle that writes the outcome
//     afterwards; the two are linked by [Entry.Attempt]. A caller that crashes
//     between them leaves an attempt with no outcome, which is exactly the
//     evidence you want.
//   - **Append-only, and tamper-evident.** There is no Update and no Delete on
//     [Log]. Each entry carries the hash of the one before it, so a deleted or
//     edited line breaks the chain and [Verify] says where.
//   - **Nothing here is a secret.** [Entry] has no field a secret fits in, the
//     same way vault.Entry has none. The vault's own listing is the model: the
//     only way to a plaintext secret is a function named to be obvious in
//     review, and this package has no such function at all.
//
// A failed append is a failed mutation. The API refuses a vault write it cannot
// record, because an unrecorded vault write is precisely the thing this file
// exists to make impossible.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Action is what was attempted. The vocabulary is closed on purpose: a console
// that has to render an unknown verb cannot say what happened.
type Action string

const (
	// Credentials — MEMORY.md §6.
	ActionCredentialAdd      Action = "credential.add"
	ActionCredentialValidate Action = "credential.validate"
	ActionCredentialRotate   Action = "credential.rotate"
	ActionCredentialRevoke   Action = "credential.revoke"
	// ActionCredentialRead is a secret leaving the vault for a real call. The
	// console can never cause one — it holds no path to a plaintext secret — so
	// an entry with this action and a console actor is itself the alarm.
	ActionCredentialRead Action = "credential.read"

	ActionProposalAccept  Action = "credential.proposal.accept"
	ActionProposalDismiss Action = "credential.proposal.dismiss"

	// Connectors and MCP — ORCHESTRATOR.md §4b, MEMORY.md §7.
	ActionConnectorGrant  Action = "connector.grant"
	ActionConnectorRevoke Action = "connector.revoke"
	ActionMCPRegister     Action = "mcp.register"
	ActionMCPRemove       Action = "mcp.remove"

	// Answering §4b's proposal, either way. The accept is distinct from
	// ActionConnectorGrant rather than folded into it: the grant is what
	// changed, and this is the question that was asked — "granted from the
	// connectors screen" and "granted by saying yes to a suggestion" are
	// different stories about the same row, which is the reason
	// GrantRequest.From exists at all.
	ActionConnectorProposalAccept Action = "connector.proposal.accept"
	// A dismissal changes nothing and is still a decision worth a line: it
	// silences a connector for a month, and a month of silence nobody can
	// account for is exactly what the log is for.
	ActionConnectorProposalDismiss Action = "connector.proposal.dismiss"
)

// Actions lists every action, for the console's filter.
func Actions() []Action {
	return []Action{
		ActionCredentialAdd, ActionCredentialValidate, ActionCredentialRotate,
		ActionCredentialRevoke, ActionCredentialRead,
		ActionProposalAccept, ActionProposalDismiss,
		ActionConnectorGrant, ActionConnectorRevoke,
		ActionConnectorProposalAccept, ActionConnectorProposalDismiss,
		ActionMCPRegister, ActionMCPRemove,
	}
}

// Mutation reports whether an action changes state. Everything but a read does.
func (a Action) Mutation() bool { return a != ActionCredentialRead }

// Outcome is how it ended.
type Outcome string

const (
	// OutcomeAttempted is written before the work runs. It is never rewritten:
	// the completion is a second entry pointing back at it.
	OutcomeAttempted Outcome = "attempted"
	OutcomeOK        Outcome = "ok"
	// OutcomeDenied is a request the authorization layer refused. It is separate
	// from failed because "somebody without the vault scope tried to add a key"
	// and "the keychain was locked" are different events.
	OutcomeDenied Outcome = "denied"
	OutcomeFailed Outcome = "failed"
)

// Actor is who. Kind is the surface the request came through, not the person:
// one box, one person (SYSTEM.md §5 has no user table), so identity here is
// about which door was used.
type Actor struct {
	// Kind is console | phone | orchestrator | runtime | installer | cloud.
	Kind string `json:"kind"`
	// ID is the account on the cloud tier, and "local" behind the printed token.
	ID string `json:"id,omitempty"`
	// Runtime names the agent runtime when one of the five caused this.
	Runtime string `json:"runtime,omitempty"`
	// From is the network origin as the server saw it. On the cloud tier that is
	// the proxy's forwarded address; self-hosted it is a loopback port.
	From string `json:"from,omitempty"`
	// Agent is a trimmed user-agent, because "which browser" is the difference
	// between the user and something running as the user.
	Agent string `json:"agent,omitempty"`
}

// Entry is one line of the log. Nothing in this struct is a secret, and the
// test in this package that round-trips an entry built from a known secret is
// what fails if a field is ever added that one fits in.
type Entry struct {
	ID  string    `json:"id"`
	At  time.Time `json:"at"`
	Seq int64     `json:"seq"`

	Actor  Actor  `json:"actor"`
	Action Action `json:"action"`

	// Target is the thing acted on: a credential id, a connector name, an MCP
	// server key. Never the thing's value.
	Target  string `json:"target,omitempty"`
	Service string `json:"service,omitempty"`

	Outcome Outcome `json:"outcome"`
	// Reason is why it was denied or how it failed, in the words of whatever
	// said no.
	Reason string `json:"reason,omitempty"`

	// Detail carries the small facts a console renders next to the line — the
	// provenance kind of a new credential, which runtimes a revoke reached.
	Detail map[string]string `json:"detail,omitempty"`

	// Attempt links an outcome back to the attempt that preceded it. An attempt
	// with no matching outcome is a mutation that never finished, and the
	// console shows it as one.
	Attempt string `json:"attempt,omitempty"`

	// Prev and Hash are the tamper-evidence chain. Prev is the previous entry's
	// Hash; Hash covers every other field including Prev.
	Prev string `json:"prev,omitempty"`
	Hash string `json:"hash,omitempty"`
}

// Pending reports whether this entry is an attempt still waiting for its
// outcome.
func (e Entry) Pending() bool { return e.Outcome == OutcomeAttempted }

// Filter narrows a read. The zero value returns the most recent DefaultLimit
// entries.
type Filter struct {
	// Limit is how many of the most recent matching entries to return.
	Limit int
	// Action, when set, keeps only that action.
	Action Action
	// Target, when set, keeps only entries about that credential or connector.
	Target string
	// Since, when set, keeps only entries at or after it.
	Since time.Time
	// Outcomes, when set, keeps only those outcomes.
	Outcomes []Outcome
}

// DefaultLimit bounds an unfiltered read. The console paginates; a support
// session does not want the whole file in one response.
const DefaultLimit = 200

// MaxLimit is the hard ceiling on one read.
const MaxLimit = 5000

func (f Filter) limit() int {
	switch {
	case f.Limit <= 0:
		return DefaultLimit
	case f.Limit > MaxLimit:
		return MaxLimit
	default:
		return f.Limit
	}
}

func (f Filter) match(e Entry) bool {
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if f.Target != "" && e.Target != f.Target {
		return false
	}
	if !f.Since.IsZero() && e.At.Before(f.Since) {
		return false
	}
	if len(f.Outcomes) > 0 {
		ok := false
		for _, o := range f.Outcomes {
			if e.Outcome == o {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Log is an append-only record. There is deliberately no Update and no Delete:
// the whole value of this thing is that it can only grow.
type Log interface {
	// Append writes one entry and returns it as written, with its id, sequence
	// and hash filled in. An error here means the mutation must not proceed.
	Append(ctx context.Context, e Entry) (Entry, error)

	// List returns matching entries, oldest first.
	List(ctx context.Context, f Filter) ([]Entry, error)

	// Durable reports whether entries survive a restart. A memory log is a
	// supported state — it keeps the mutation path honest on a box with no
	// writable data directory — but the console has to be able to say so rather
	// than implying a file that is not there.
	Durable() bool

	// Path is where the log lives, for the console to print. Empty when there is
	// no file.
	Path() string

	Close() error
}

// Errors.
var (
	// ErrNoLog is a mutation with nowhere to record it. Callers turn this into a
	// refusal, not a warning.
	ErrNoLog = errors.New("audit: no log configured")
	// ErrBroken is a chain whose hashes do not line up.
	ErrBroken = errors.New("audit: the chain is broken")
)

// ------------------------------------------------------------------ chain --

// seal fills in the identity and hash of an entry, given the previous hash.
func seal(e Entry, prev string, seq int64, now func() time.Time, newID func() string) Entry {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = now()
	}
	e.At = e.At.UTC()
	e.Seq = seq
	e.Prev = prev
	e.Hash = ""
	e.Hash = sum(e)
	return e
}

// sum hashes an entry with its Hash field empty, so the value covers every
// other field including Prev.
func sum(e Entry) string {
	e.Hash = ""
	b, err := json.Marshal(canonical(e))
	if err != nil {
		// Marshalling a struct of strings and times cannot fail; if it somehow
		// does, a hash that never verifies is safer than one that always does.
		return "unhashable"
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// canonical renders an entry deterministically. encoding/json already sorts map
// keys, so the only thing to pin is the time format.
func canonical(e Entry) []string {
	keys := make([]string, 0, len(e.Detail))
	for k := range e.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	detail := make([]string, 0, len(keys))
	for _, k := range keys {
		detail = append(detail, k+"="+e.Detail[k])
	}
	return []string{
		e.ID,
		e.At.UTC().Format(time.RFC3339Nano),
		fmt.Sprint(e.Seq),
		e.Actor.Kind, e.Actor.ID, e.Actor.Runtime, e.Actor.From, e.Actor.Agent,
		string(e.Action), e.Target, e.Service,
		string(e.Outcome), e.Reason, e.Attempt, e.Prev,
		strings.Join(detail, "\x00"),
	}
}

// Verify walks a whole chain from its start and reports the first entry whose
// hash or link does not hold. Entries must be in write order.
func Verify(entries []Entry) error { return VerifyFrom("", entries) }

// VerifyFrom verifies a window of the chain that begins after prev.
//
// It exists because the console reads the most recent few hundred entries, and
// verifying those against a chain that starts thousands of entries earlier would
// report every ordinary log as broken. A window is internally consistent or it
// is not, and that is a real answer: an edited line inside it still fails.
func VerifyFrom(prev string, entries []Entry) error {
	for i, e := range entries {
		if e.Prev != prev {
			return fmt.Errorf("%w: entry %d (%s) follows %q, not %q", ErrBroken, i, e.ID, e.Prev, prev)
		}
		if got := sum(e); got != e.Hash {
			return fmt.Errorf("%w: entry %d (%s) hashes to %s, not %s", ErrBroken, i, e.ID, got, e.Hash)
		}
		prev = e.Hash
	}
	return nil
}

// ---------------------------------------------------------------- attempts --

// Attempt is a mutation in flight: the attempt is already on disk and the
// outcome is not written yet.
type Attempt struct {
	log   Log
	entry Entry
	done  bool
}

// Begin records the attempt. A non-nil error means nothing was recorded, and
// the caller must refuse the mutation rather than performing it unlogged.
//
// The entry's Outcome is ignored — an attempt is always OutcomeAttempted.
func Begin(ctx context.Context, l Log, e Entry) (*Attempt, error) {
	if l == nil {
		return nil, ErrNoLog
	}
	e.Outcome = OutcomeAttempted
	e.Attempt = ""
	written, err := l.Append(ctx, e)
	if err != nil {
		return nil, err
	}
	return &Attempt{log: l, entry: written}, nil
}

// ID is the attempt's entry id, which its outcome points back at.
func (a *Attempt) ID() string {
	if a == nil {
		return ""
	}
	return a.entry.ID
}

// OK records success. detail may be nil.
func (a *Attempt) OK(ctx context.Context, detail map[string]string) error {
	return a.finish(ctx, OutcomeOK, "", detail)
}

// Fail records a mutation that was allowed and did not work.
func (a *Attempt) Fail(ctx context.Context, err error) error {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	return a.finish(ctx, OutcomeFailed, reason, nil)
}

// Deny records a mutation that was refused before it ran.
func (a *Attempt) Deny(ctx context.Context, reason string) error {
	return a.finish(ctx, OutcomeDenied, reason, nil)
}

func (a *Attempt) finish(ctx context.Context, o Outcome, reason string, detail map[string]string) error {
	if a == nil || a.log == nil {
		return ErrNoLog
	}
	if a.done {
		// Writing a second outcome for one attempt would put two contradictory
		// facts in a log whose value is that it is not contradictory.
		return errors.New("audit: this attempt already has an outcome")
	}
	a.done = true

	// At is left zero so the log stamps the outcome with its own time. The gap
	// between an attempt and its outcome is how long a locked keychain took to
	// answer, and copying the attempt's timestamp would erase it.
	out := Entry{
		Actor:   a.entry.Actor,
		Action:  a.entry.Action,
		Target:  a.entry.Target,
		Service: a.entry.Service,
		Outcome: o,
		Reason:  reason,
		Detail:  detail,
		Attempt: a.entry.ID,
	}
	_, err := a.log.Append(ctx, out)
	return err
}

// NewID is the id generator both implementations use.
func NewID() string { return uuid.NewString() }
