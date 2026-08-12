package api

import (
	"context"
	"net/http"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// DASHBOARD.md §3.5 — runtimes and health.
//
// "Which runtimes are installed, authenticated, and running. Which model each
// is configured for. The voice provider. The two orchestrator models from
// ORCHESTRATOR.md §2b, with a re-probe button — because 'my glasses stopped
// talking' is almost always an expired credential, and the fastest support
// answer is a page that already says which one."
//
// That last clause is the design. The probe result is cached on the server so
// the health page can *already say* which credential is stale, and the button
// re-runs it rather than the page being the only way to find out.

// MachineSource is the detection pass behind "installed, authenticated and
// running". It is a function for the same reason [MCPSource] is: detection
// shells out to five runtimes and walks their stores, and must not happen on
// every page load.
type MachineSource func(ctx context.Context) (detect.Report, error)

// RuntimeAuth is whether one runtime can currently reach its provider.
//
// It is a separate seam from [MachineSource] because it is a different kind of
// question: detection reads the filesystem, and this needs a real call or a
// token inspection per runtime. Nothing implements it yet, and until something
// does the console shows "unknown" rather than a guess.
type RuntimeAuth struct {
	OK        bool   `json:"ok"`
	Note      string `json:"note,omitempty"`
	CheckedAt int64  `json:"checked_at,omitempty"`
}

// RuntimeAuthSource reports the login state of each runtime, keyed by runtime
// id. A runtime absent from the map stays unknown.
type RuntimeAuthSource func(ctx context.Context) (map[string]RuntimeAuth, error)

// unknownAuthNote is what the console prints instead of a green or a red tick.
const unknownAuthNote = "nothing on this machine observes whether this runtime is logged in, " +
	"so this is unknown rather than a claim either way"

// HostHealthSource is cloud-only machine health. DASHBOARD.md §3.5: "Cloud tier
// adds machine health: uptime, disk, last backup."
type HostHealthSource func(ctx context.Context) (HostHealth, error)

// HostHealth is the cloud box.
type HostHealth struct {
	UptimeSec  int64  `json:"uptime_sec,omitempty"`
	DiskFree   int64  `json:"disk_free_bytes,omitempty"`
	DiskTotal  int64  `json:"disk_total_bytes,omitempty"`
	LastBackup int64  `json:"last_backup,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Setup is what the installer chose: the two orchestrator models, the voice,
// the embedder. Names and credential *references* only — never a secret, the
// same rule config.toml itself follows.
type Setup struct {
	// Small is ORCHESTRATOR.md §2b's voice model, Big the one that does the work
	// and holds the MCP registry and a shell.
	Small ModelSetup `json:"small"`
	Big   ModelSetup `json:"big"`

	Voice     VoiceSetup     `json:"voice"`
	Embedding EmbeddingSetup `json:"embedding"`
}

// ModelSetup is one configured model.
type ModelSetup struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	API    string `json:"api,omitempty"`
	// Credential is the reference — "env:OPENROUTER_API_KEY", "vault:<id>" —
	// never the secret behind it.
	Credential string `json:"credential,omitempty"`
}

// VoiceSetup is ORCHESTRATOR.md §2a's choice plus its keyless fallback, which
// is never empty because "mute out of the box" is the worst possible first hour
// for a voice product.
type VoiceSetup struct {
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Credential string `json:"credential,omitempty"`
	Fallback   string `json:"fallback,omitempty"`
}

// EmbeddingSetup is the third peer. Local by default, and the console says so,
// because on the self-hosted tier that is the whole argument.
type EmbeddingSetup struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Dims     int    `json:"dims,omitempty"`
	Local    bool   `json:"local"`
}

// ---------------------------------------------------------------- probing --

// Prober re-runs ORCHESTRATOR.md §2's credential probes: one real call each.
type Prober interface {
	Probe(ctx context.Context) Probes
}

// ProberFunc adapts a function to [Prober].
type ProberFunc func(ctx context.Context) Probes

// Probe implements [Prober].
func (f ProberFunc) Probe(ctx context.Context) Probes { return f(ctx) }

// Probes is one pass over every credential the orchestrator needs.
//
// The two maps are the packages' own result types rather than a re-declaration,
// so a reason code cannot drift between the installer's output and the
// console's: llm.Pair.Probe returns exactly this map, and voice.ProbePlan
// exactly this slice.
type Probes struct {
	Models map[string]llm.ProbeResult
	Voice  []voice.Check
}

// ProbeReport is [Probes] on the wire.
type ProbeReport struct {
	At     int64        `json:"at"`
	Models []ModelProbe `json:"models"`
	Voice  []VoiceProbe `json:"voice"`
	// OK is false when anything the orchestrator needs is not working. It is the
	// one field a support answer starts from.
	OK bool `json:"ok"`
}

// ModelProbe is one orchestrator model's answer.
type ModelProbe struct {
	// Role is "small" or "big" — the voice and the work (ORCHESTRATOR.md §3b).
	Role   string `json:"role"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// Reason is llm.Reason: ok | missing_credential | expired | unresolved_ref |
	// unavailable. "Expired" is reserved for 401/403/402 — calling a 404 expired
	// would send somebody to rotate a working key.
	Reason string `json:"reason"`
	// Detail is the provider's own error, verbatim. Empirical beats maintained.
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Ref       string `json:"ref,omitempty"`
	At        int64  `json:"at"`
	OK        bool   `json:"ok"`
}

// VoiceProbe is one voice option's answer.
type VoiceProbe struct {
	Option string `json:"option"`
	Label  string `json:"label,omitempty"`
	// Probed is false where a row cannot be tested from this machine at all —
	// phone-native synthesis happens on the handset. Reporting that as ok would
	// claim a verification that never happened.
	Probed    bool   `json:"probed"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	At        int64  `json:"at"`
	OK        bool   `json:"ok"`
}

func probeReport(p Probes, at time.Time) ProbeReport {
	out := ProbeReport{At: at.UnixMilli(), Models: []ModelProbe{}, Voice: []VoiceProbe{}, OK: true}
	// Fixed order: small then big, so the page does not reshuffle between loads.
	for _, role := range []string{"small", "big"} {
		r, ok := p.Models[role]
		if !ok {
			continue
		}
		m := ModelProbe{
			Role: role, Vendor: r.Vendor, Model: r.Model,
			Reason: string(r.Reason), Detail: r.Detail,
			LatencyMS: r.Latency.Milliseconds(), Ref: r.Ref,
			At: msOrZero(r.At), OK: r.OK(),
		}
		if !m.OK {
			out.OK = false
		}
		out.Models = append(out.Models, m)
	}
	for _, c := range p.Voice {
		v := VoiceProbe{
			Option: c.Option, Label: c.Label, Probed: c.Probed,
			Reason: string(c.Reason), Detail: c.Detail, Bytes: c.Bytes,
			LatencyMS: c.Latency.Milliseconds(), At: msOrZero(c.At), OK: c.OK(),
		}
		if !v.OK {
			out.OK = false
		}
		out.Voice = append(out.Voice, v)
	}
	return out
}

// handleProbe is the re-probe button.
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if s.prober == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable,
			"no model client is wired on this machine, so there is nothing to re-probe. "+
				"The installer configures the two orchestrator models and the voice (ORCHESTRATOR.md §2)")
		return
	}
	// The probe makes real calls to two model providers and a speech provider,
	// each capped at 30s upstream. The request context bounds the whole thing.
	p := s.prober.Probe(r.Context())
	rep := probeReport(p, s.now())

	s.mu.Lock()
	s.lastProbe = &rep
	s.mu.Unlock()

	s.publish(ConsoleEvent{Kind: ConsoleProbe, Action: "probe", Outcome: outcomeWord(rep.OK)})
	writeJSON(w, http.StatusOK, rep)
}

func outcomeWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func (s *Server) probeSnapshot() *ProbeReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastProbe
}

// ------------------------------------------------------------- detection --

// runtimeAuth reads the login state of each runtime, or nothing at all.
func (s *Server) runtimeAuth(ctx context.Context) map[string]RuntimeAuth {
	if s.runtimeAuthn == nil {
		return nil
	}
	got, err := s.runtimeAuthn(ctx)
	if err != nil {
		s.log.Warn("api: runtime auth check", "error", err)
		return nil
	}
	return got
}

// runtimeDetail is what detection knows about the five runtimes, keyed by
// runtime id. An error is not fatal: DASHBOARD.md §3.5 is a support page, and a
// page that renders nothing because one probe failed is worse than one that
// renders what it has.
func (s *Server) runtimeDetail(ctx context.Context) (map[string]detect.Finding, string) {
	if s.machine == nil {
		return nil, "no detection pass has run on this machine, so 'installed' is unknown rather than false"
	}
	rep, err := s.machine(ctx)
	if err != nil {
		return nil, "detection failed: " + err.Error()
	}
	out := map[string]detect.Finding{}
	for _, f := range rep.Findings {
		out[string(f.Runtime)] = f
	}
	return out, ""
}

// modelsInUse reads the model each runtime's most recent session ran on.
//
// There is no per-runtime model in the config: a runtime is driven with
// whatever model the session was started with (registry.StartOptions.Model,
// stored as session.agent). So the honest answer to "which model is this
// runtime configured for" is the last one it actually used, and an empty string
// where it has never run.
func (s *Server) modelsInUse(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.db == nil {
		return out
	}
	// SQLite's bare-column rule: with max() in the select list, the other bare
	// columns come from the row that max() picked. That is the whole query.
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT runtime, agent, MAX(last_active) FROM session
		WHERE agent <> '' GROUP BY runtime`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var runtime, agent string
		var lastActive int64
		if err := rows.Scan(&runtime, &agent, &lastActive); err != nil {
			return out
		}
		out[runtime] = agent
	}
	return out
}

// fillRuntime adds the detection half of a runtime row.
func fillRuntime(st *RuntimeState, f detect.Finding, ok bool) {
	if !ok {
		return
	}
	st.Detected = true
	st.Installed = f.Installed
	st.Status = f.Status().String()
	st.StatusLine = f.Status().Line()
	st.Version = f.Version
	st.VersionNote = f.VersionNote
	st.BinaryPath = f.BinaryPath
	st.StateDir = f.StateDir
	// Where the state directory came from matters more than the path: a path we
	// were told is a fact, a path we assumed is a guess, and conflating the two
	// is exactly the OpenClaw trap MEMORY.md §4 names.
	st.StateDirSource = string(f.StateDirSource)
	st.StateDirTrusted = f.StateDirSource.Trusted()
	st.Running = len(f.Running)
	st.Notes = append(st.Notes, f.Notes...)
	if n, known := f.SessionCount(); known {
		st.Stored = &n
	} else if f.SessionsNote != "" {
		st.Notes = append(st.Notes, f.SessionsNote)
	}
	if b, known := f.Bytes(); known {
		st.StoreBytes = &b
	}
}

func (s *Server) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	states, note := s.runtimeStates(r.Context())
	out := map[string]any{"runtimes": states, "at": s.now().UnixMilli()}
	if note != "" {
		out["note"] = note
	}
	writeJSON(w, http.StatusOK, out)
}

// runtimeStates assembles one row per runtime out of three sources: what the
// adapter layer can drive, what detection found on disk, and what the registry
// has actually run.
func (s *Server) runtimeStates(ctx context.Context) ([]RuntimeState, string) {
	found, note := s.runtimeDetail(ctx)
	models := s.modelsInUse(ctx)
	auth := s.runtimeAuth(ctx)
	counts := map[string]int{}
	if rows, err := s.reg.List(ctx, storeAll()); err == nil {
		for _, row := range rows {
			counts[row.Runtime]++
		}
	}

	out := make([]RuntimeState, 0, len(adapter.Runtimes()))
	for _, rt := range adapter.Runtimes() {
		st := RuntimeState{
			Runtime:  string(rt),
			Protocol: string(rt.Protocol()),
			Sessions: counts[string(rt)],
			Model:    models[string(rt)],
		}
		a, ok := s.reg.Adapter(rt)
		st.Adapter = ok
		caps := adapter.Baseline(rt)
		if ok {
			caps = a.Capabilities()
		}
		st.Notes = nil
		st.Missing = nil
		st.CapabilityNotes = map[string]string{}
		for _, c := range caps.Missing() {
			st.Missing = append(st.Missing, string(c))
			if n := caps.Note(c); n != "" {
				st.CapabilityNotes[string(c)] = n
			}
		}
		if len(st.CapabilityNotes) == 0 {
			st.CapabilityNotes = nil
		}
		f, ok := found[string(rt)]
		fillRuntime(&st, f, ok)
		if a, known := auth[string(rt)]; known {
			okCopy := a.OK
			st.Authenticated = &okCopy
			st.AuthNote = a.Note
		} else {
			st.AuthNote = unknownAuthNote
		}
		out = append(out, st)
	}
	return out, note
}
