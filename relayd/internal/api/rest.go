package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Health is what DASHBOARD.md §3.5 renders: which runtimes are installed and
// running, what is degraded, and what has recently gone wrong.
//
// "My glasses stopped talking" is almost always an expired credential or a
// runtime that will not start, and the fastest support answer is a page that
// already says which one.
type Health struct {
	OK        bool                `json:"ok"`
	Version   string              `json:"version,omitempty"`
	StartedAt int64               `json:"started_at"`
	UptimeSec int64               `json:"uptime_sec"`
	Listen    string              `json:"listen"`
	LAN       bool                `json:"lan"`
	Sessions  SessionCounts       `json:"sessions"`
	Runtimes  []RuntimeState      `json:"runtimes"`
	Bus       BusStats            `json:"bus"`
	Pings     PingStats           `json:"pings"`
	Clients   int                 `json:"clients"`
	Incidents []registry.Incident `json:"incidents"`

	// Vault says where the secrets actually live and whether the OS keychain was
	// available, so the console shows honest degradation rather than implying
	// protection that is not there. Nil when no vault is open.
	Vault *VaultStatus `json:"vault,omitempty"`

	// Audit is DASHBOARD.md §4's trail, and specifically whether it is durable.
	Audit AuditHealth `json:"audit"`

	// Subsystems is what this daemon actually wired, keyed by name, valued
	// either "on" or the reason it is not.
	//
	// It exists because the failure this codebase keeps producing is a
	// subsystem that is fully built, fully tested and constructed by nothing:
	// the suite stays green and the product does nothing, and there is no
	// symptom until somebody notices the feature never happens. A daemon that
	// reports what it wired turns that into a line on the health screen —
	// "compaction: no work model configured" is a fact a user can act on, and
	// its absence is a bug a test can catch.
	Subsystems map[string]string `json:"subsystems,omitempty"`

	// Setup is the installer's choices: the two orchestrator models, the voice,
	// the embedder. References, never secrets.
	Setup *Setup `json:"setup,omitempty"`

	// Probe is the last credential probe, cached so the health page already says
	// which credential is stale rather than the button being the only way to
	// find out. Nil until something has probed.
	Probe *ProbeReport `json:"probe,omitempty"`

	// Machine is cloud-only: uptime, disk, last backup.
	Machine *HostHealth `json:"machine,omitempty"`

	// Note carries anything that made this report less complete than it looks —
	// chiefly "no detection pass has run", which is why every runtime says
	// detected:false.
	Note string `json:"note,omitempty"`
}

// AuditHealth is whether the evidence survives a restart.
type AuditHealth struct {
	Durable bool   `json:"durable"`
	Path    string `json:"path,omitempty"`
	Note    string `json:"note,omitempty"`
}

// SessionCounts is the list, summarised.
type SessionCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Awaiting int `json:"awaiting"`
	Idle     int `json:"idle"`
	Closed   int `json:"closed"`
	Live     int `json:"live"`
}

// RuntimeState is one of the five, on DASHBOARD.md §3.5's screen.
//
// Three sources meet in this struct and the field names keep them apart,
// because conflating them is how a support page starts lying: what the adapter
// layer can *drive*, what detection found on *disk*, and what the registry has
// actually *run*.
type RuntimeState struct {
	Runtime  string `json:"runtime"`
	Protocol string `json:"protocol"`
	// Adapter is whether relayd can drive this runtime at all right now.
	Adapter bool `json:"adapter"`
	// Missing is what this runtime cannot be observed to do. It is rendered as
	// fact, not as a bug: ACP has no cost field anywhere in its protocol, and a
	// console that shows a zero there is lying.
	Missing         []string          `json:"missing,omitempty"`
	CapabilityNotes map[string]string `json:"capability_notes,omitempty"`
	Sessions        int               `json:"sessions"`

	// Model is the model this runtime last actually ran on. There is no
	// per-runtime model setting — a session is started with one — so the honest
	// answer is the most recent, and empty where it has never run.
	Model string `json:"model,omitempty"`

	// Detected is false when no detection pass has run. Everything below it is
	// then unknown rather than false, which is a distinction MEMORY.md §1 is
	// emphatic about: "installed but never used" and "not installed" are both
	// normal, and "we could not tell" is neither.
	Detected bool `json:"detected"`
	// Installed is a binary on PATH; Status is absent | never_run | in_use |
	// history_only, and StatusLine is the clause the console prints.
	Installed  bool   `json:"installed"`
	Status     string `json:"status,omitempty"`
	StatusLine string `json:"status_line,omitempty"`

	Version     string `json:"version,omitempty"`
	VersionNote string `json:"version_note,omitempty"`
	BinaryPath  string `json:"binary_path,omitempty"`

	StateDir string `json:"state_dir,omitempty"`
	// StateDirSource is env | asked | config | profile | default, and Trusted is
	// whether that counts as authoritative. Never hardcode ~/.openclaw: a reader
	// that assumes the default silently reports an empty history as success.
	StateDirSource  string `json:"state_dir_source,omitempty"`
	StateDirTrusted bool   `json:"state_dir_trusted,omitempty"`

	// Authenticated is DASHBOARD.md §3.5's third word, and it is a tri-state on
	// purpose. Nothing in internal/detect observes whether a runtime is logged
	// in — the five keep their credentials in five different places and three of
	// them are OAuth — so nil means *unknown* and AuthNote says why. A false here
	// would be a claim, and "your Claude Code is logged out" is exactly the kind
	// of claim that sends somebody to re-run a login that was fine.
	Authenticated *bool  `json:"authenticated"`
	AuthNote      string `json:"auth_note,omitempty"`

	// Running is how many processes on this machine look like this runtime.
	Running int `json:"running"`
	// Stored is the session count in the runtime's own store, nil where nobody
	// counted. Rendering "0 sessions" for a store we never opened is the same
	// class of lie as an adapter emitting an event it did not see.
	Stored     *int   `json:"stored_sessions"`
	StoreBytes *int64 `json:"store_bytes,omitempty"`

	// Notes are everything detection had to derive or could not observe.
	Notes []string `json:"notes,omitempty"`
}

// BusStats is the event bus, for the health page.
type BusStats struct {
	Published   uint64 `json:"published"`
	Subscribers int    `json:"subscribers"`
}

// PingStats mirrors bus.PingStats over the wire.
type PingStats struct {
	Blocking      uint64 `json:"blocking"`
	Informational uint64 `json:"informational"`
	Repings       uint64 `json:"repings"`
	Retracted     uint64 `json:"retracted"`
	Withdrawn     uint64 `json:"withdrawn"`
	Batched       uint64 `json:"batched"`
	Failed        uint64 `json:"failed"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.reg.List(ctx, store.SessionFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}

	h := Health{
		OK:         true,
		Version:    s.version,
		StartedAt:  s.startedAt.UnixMilli(),
		UptimeSec:  int64(s.now().Sub(s.startedAt) / time.Second),
		Listen:     s.listen,
		LAN:        s.lan,
		Clients:    s.Clients(),
		Incidents:  recentIncidents(s.reg.Incidents(), 50),
		Subsystems: s.subsystemReport(),
	}
	for _, row := range rows {
		h.Sessions.Total++
		switch row.State {
		case store.SessionRunning:
			h.Sessions.Running++
		case store.SessionAwaiting:
			h.Sessions.Awaiting++
		case store.SessionIdle:
			h.Sessions.Idle++
		case store.SessionClosed:
			h.Sessions.Closed++
		}
	}
	h.Sessions.Live = len(s.reg.Live())

	h.Runtimes, h.Note = s.runtimeStates(ctx)

	if b := s.reg.Bus(); b != nil {
		h.Bus = BusStats{Published: b.Published(), Subscribers: b.Subscribers()}
	}
	if pg := s.currentPinger(); pg != nil {
		p := pg.Stats()
		h.Pings = PingStats{
			Blocking: p.Blocking, Informational: p.Informational, Repings: p.Repings,
			Retracted: p.Retracted, Withdrawn: p.Withdrawn, Batched: p.Batched, Failed: p.Failed,
		}
	}

	// The vault's own state, said out loud rather than implying a keychain that
	// is not there (MEMORY.md §6's degraded path).
	if s.credentials != nil {
		v := vaultStatus(s.credentials)
		h.Vault = &v
	}
	h.Audit = AuditHealth{
		Durable: s.audit != nil && s.audit.Durable(),
		Path:    auditPath(s.audit),
	}
	if !h.Audit.Durable {
		h.Audit.Note = "this machine is not keeping a durable audit trail — " +
			"credential and connector mutations are recorded in memory and lost on restart"
	}
	h.Setup = s.setup
	h.Probe = s.probeSnapshot()

	if s.cloud && s.host != nil {
		if hh, err := s.host(ctx); err == nil {
			h.Machine = &hh
		} else {
			h.Machine = &HostHealth{Note: err.Error()}
		}
	}
	writeJSON(w, http.StatusOK, h)
}

func auditPath(l audit.Log) string {
	if l == nil {
		return ""
	}
	return l.Path()
}

// storeAll is the unfiltered session filter, named so the two callers that want
// "everything" read as though they mean it.
func storeAll() store.SessionFilter { return store.SessionFilter{} }

func recentIncidents(all []registry.Incident, n int) []registry.Incident {
	if len(all) <= n {
		if all == nil {
			return []registry.Incident{}
		}
		return all
	}
	return all[len(all)-n:]
}

// sessionList is the one place a registry-tier list is assembled, so the
// phone's session.list frame and the console's GET /v1/sessions cannot drift
// apart. The console's endpoint unions this with the index tier — see
// sessions.go — because DASHBOARD.md §3.1 wants live *and* historical.
func (s *Server) sessionList(ctx context.Context, f store.SessionFilter) (SessionList, error) {
	rows, err := s.reg.List(ctx, f)
	if err != nil {
		return SessionList{}, err
	}
	out := SessionList{At: s.now().UnixMilli(), Sessions: make([]SessionSummary, 0, len(rows))}
	for _, row := range rows {
		sum := SessionSummary{
			ID:         row.ID,
			NativeID:   row.NativeID,
			Runtime:    row.Runtime,
			Subject:    row.Subject,
			Workspace:  row.Workspace,
			Model:      row.Agent,
			State:      string(row.State),
			LastActive: row.LastActive.UnixMilli(),
			CreatedAt:  row.CreatedAt.UnixMilli(),
			CostUSD:    row.CostUSD,
			Tokens:     row.TokensTotal,
			Blocked:    row.State == store.SessionAwaiting,
			Source:     SourceRegistry,
		}
		if e, ok := s.reg.Get(row.ID); ok {
			sum.Live = true
			sum.Questions = len(e.Questions())
		}
		out.Sessions = append(out.Sessions, sum)
	}
	// DASHBOARD.md §3.1: blocked sessions at the top, unmissable. A blocked
	// session is the one failure mode that silently stops all work, so it does
	// not get sorted by recency along with everything else.
	hoistBlocked(out.Sessions)
	return out, nil
}

func hoistBlocked(ss []SessionSummary) {
	blocked := make([]SessionSummary, 0, len(ss))
	rest := make([]SessionSummary, 0, len(ss))
	for _, v := range ss {
		if v.Blocked {
			blocked = append(blocked, v)
		} else {
			rest = append(rest, v)
		}
	}
	copy(ss, append(blocked, rest...))
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := sessionKey(r)

	d, err := s.reg.Detail(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		// A session the registry never drove is not necessarily a session that
		// never existed: the index tier holds one row for every session that ever
		// has, across all five runtimes. Falling back to it is what makes a
		// historical row on DASHBOARD.md §3.1's list clickable.
		if out, ok := s.indexDetail(r.Context(), id); ok {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusNotFound, CodeNoSuchSession, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	out := sessionDetail(d)
	if ref, ok, err := s.transcriptFor(r.Context(), id); err == nil && ok {
		out.Transcript = &ref
	}
	writeJSON(w, http.StatusOK, out)
}

// indexDetail assembles what the historical tier knows about one session.
//
// There are no turns and no tool calls: MEMORY.md §3 keeps a pointer, not a
// copy, so the exchange itself lives in the runtime's own file and is reached
// through the transcript range read. Returning empty slices rather than nothing
// is deliberate — the screen renders the same way for both tiers, and the
// transcript link is what fills it.
func (s *Server) indexDetail(ctx context.Context, id string) (SessionDetail, bool) {
	if s.db == nil {
		return SessionDetail{}, false
	}
	runtime, native, ok := strings.Cut(id, "/")
	if !ok {
		return SessionDetail{}, false
	}
	rows, err := s.indexSessions(ctx, SessionQuery{Runtime: runtime, Limit: maxIndexRows})
	if err != nil {
		return SessionDetail{}, false
	}
	for _, v := range rows {
		if v.NativeID != native {
			continue
		}
		return SessionDetail{
			Session:    v,
			Turns:      []TurnView{},
			Tools:      []ToolCallView{},
			Transcript: v.Transcript,
		}, true
	}
	return SessionDetail{}, false
}

// SessionDetail is one session, with what the console shows beside it.
type SessionDetail struct {
	Session      SessionSummary      `json:"session"`
	Turns        []TurnView          `json:"turns"`
	Tools        []ToolCallView      `json:"tools"`
	Live         bool                `json:"live"`
	Capabilities map[string]string   `json:"capabilities,omitempty"`
	Missing      []string            `json:"missing,omitempty"`
	Questions    []registry.Question `json:"questions,omitempty"`
	// Transcript is a POINTER into the runtime's own file, never a copy
	// (MEMORY.md §3). It is filled in by the index once backfill has run, and it
	// is what GET /v1/sessions/{id}/transcript range-reads. Nil means backfill
	// has not seen this session, which is different from "there is no
	// transcript".
	Transcript *TranscriptRef `json:"transcript,omitempty"`
}

// TurnView is one exchange.
type TurnView struct {
	ID         string   `json:"id"`
	Role       string   `json:"role"`
	Text       string   `json:"text"`
	At         int64    `json:"at"`
	OK         bool     `json:"ok"`
	StopReason string   `json:"stop_reason,omitempty"`
	DurationMS int64    `json:"duration_ms,omitempty"`
	CostUSD    *float64 `json:"cost_usd"`
	Tokens     *int64   `json:"tokens"`
}

// ToolCallView is one tool call. ArgsDigest is a digest and never the
// arguments — tool arguments routinely carry paths, tokens and payloads.
type ToolCallView struct {
	ID         string `json:"id"`
	Tool       string `json:"tool"`
	Target     string `json:"target,omitempty"`
	ArgsDigest string `json:"args_digest,omitempty"`
	At         int64  `json:"at"`
	Status     string `json:"status,omitempty"`
}

func sessionDetail(d registry.Detail) SessionDetail {
	out := SessionDetail{
		Session: SessionSummary{
			ID:         d.Session.ID,
			Runtime:    d.Session.Runtime,
			Subject:    d.Session.Subject,
			Workspace:  d.Session.Workspace,
			State:      string(d.Session.State),
			LastActive: d.Session.LastActive.UnixMilli(),
			CreatedAt:  d.Session.CreatedAt.UnixMilli(),
			CostUSD:    d.Session.CostUSD,
			Tokens:     d.Session.TokensTotal,
			Blocked:    d.Session.State == store.SessionAwaiting,
			Questions:  len(d.Questions),
			Live:       d.Live,
		},
		Live:         d.Live,
		Capabilities: d.Capabilities,
		Missing:      d.Missing,
		Questions:    d.Questions,
		Turns:        make([]TurnView, 0, len(d.Turns)),
		Tools:        make([]ToolCallView, 0, len(d.Tools)),
	}
	for _, t := range d.Turns {
		out.Turns = append(out.Turns, TurnView{
			ID: t.ID, Role: t.Role, Text: t.Text, At: t.At.UnixMilli(), OK: t.OK,
			StopReason: t.StopReason, DurationMS: t.Duration.Milliseconds(),
			CostUSD: t.CostUSD, Tokens: t.Tokens,
		})
	}
	for _, c := range d.Tools {
		out.Tools = append(out.Tools, ToolCallView{
			ID: c.ID, Tool: c.Tool, Target: c.Target, ArgsDigest: c.ArgsDigest,
			At: c.At.UnixMilli(), Status: c.ResultStatus,
		})
	}
	return out
}

type sendTurnRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleSendTurn(w http.ResponseWriter, r *http.Request) {
	var req sendTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	turnID, err := s.reg.Send(r.Context(), r.PathValue("id"), adapter.Turn{Text: req.Text})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"turn": turnID})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.Cancel(r.Context(), r.PathValue("id"), r.URL.Query().Get("turn")); err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type answerRequest struct {
	Question  string `json:"question,omitempty"`
	Option    string `json:"option,omitempty"`
	Decision  string `json:"decision"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Message   string `json:"message,omitempty"`
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var req answerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	d, err := decision(req.Decision)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	reply := event.Reply{
		OptionID:  req.Option,
		Decision:  d,
		Interrupt: req.Interrupt,
		Message:   req.Message,
	}
	err = s.reg.AnswerQuestion(r.Context(), r.PathValue("id"), req.Question, reply)
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decision(v string) (event.Decision, error) {
	switch event.Decision(v) {
	case event.DecisionAllow, event.DecisionDeny, event.DecisionCancelled:
		return event.Decision(v), nil
	case "":
		return event.DecisionAllow, nil
	}
	return "", errors.New("decision must be allow, deny or cancelled")
}

func (s *Server) writeRegistryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNoSuchSession), errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, CodeNoSuchSession, err.Error())
	case errors.Is(err, registry.ErrNoOpenQuestion), errors.Is(err, event.ErrAnswered),
		errors.Is(err, event.ErrWithdrawn), errors.Is(err, event.ErrUnknownOption):
		writeErr(w, http.StatusConflict, CodeFailed, err.Error())
	case errors.Is(err, adapter.ErrUnsupported):
		// Degrade visibly: the runtime cannot do this, and the console should
		// say which one and why rather than showing a generic failure.
		writeErr(w, http.StatusNotImplemented, CodeUnsupported, err.Error())
	case errors.Is(err, registry.ErrNoAdapter):
		writeErr(w, http.StatusServiceUnavailable, CodeUnsupported, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
	}
}
