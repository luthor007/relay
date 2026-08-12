// Package config is relayd's configuration file, its defaults, and where both
// live on disk. Deliberately boring.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/luthor007/relay/relayd/internal/voice"
)

// Env vars that move everything, mostly so tests and packagers can.
const (
	EnvConfigDir = "RELAY_CONFIG_DIR"
	EnvDataDir   = "RELAY_DATA_DIR"
)

// FileName is the config file's name inside the config directory.
const FileName = "config.toml"

// DefaultListen binds to loopback, not 0.0.0.0.
//
// DASHBOARD.md §4: the console can write to the vault, which makes it the
// highest-value target in the system — above the glasses and above relayd's own
// API. Anything that widens this is a decision someone makes on purpose.
const DefaultListen = "127.0.0.1:8787"

// Config is relayd's whole configuration.
type Config struct {
	// Listen is the address relayd serves the API and the console on.
	Listen string `toml:"listen"`
	// DataDir holds the databases. Empty means the platform default.
	DataDir string `toml:"data_dir"`

	Log        Log                      `toml:"log"`
	Models     Models                   `toml:"models"`
	Voice      Voice                    `toml:"voice"`
	Embedding  Embedding                `toml:"embedding"`
	Routing    Routing                  `toml:"routing"`
	Connectors Connectors               `toml:"connectors"`
	Relay      Relay                    `toml:"relay"`
	Runtimes   map[string]RuntimeConfig `toml:"runtimes"`
}

// Relay is SYSTEM.md §7's rendezvous relay, from this machine's side.
//
// Empty is a supported and common state: a box that is only ever reached from
// its own LAN needs nothing here, and the daemon says so on the health screen
// rather than pretending the feature is on. What is not supported is a URL that
// looks configured and dials nothing, which is why an unusable one is refused at
// load rather than logged at runtime.
type Relay struct {
	// URL is the relay's base, ws:// or wss://. §7's relay is one we run and is
	// free even for self-hosters, so this is normally our hostname — but it is
	// configurable because the relay is open and a user who would rather run
	// their own should not have to fork the daemon to do it.
	URL string `toml:"url"`
	// BoxID is this machine's durable name at the relay. Left empty, the daemon
	// generates one on first start and persists it beside the databases: it has
	// to survive a restart, or every reconnect would look like a new machine to
	// every phone that had paired with this one.
	//
	// It is **not a secret** and must never become one. Anyone who learns it can
	// open a socket to this daemon through the relay, and gets exactly as far as
	// a stranger on the LAN — which is nowhere, because the API authenticates.
	// Treating it as a secret would be the beginning of a system where knowing
	// an identifier is authority.
	BoxID string `toml:"box_id"`
	// MaxStreams bounds concurrent inbound streams. Zero takes the default.
	MaxStreams int `toml:"max_streams"`
}

// Enabled reports whether this machine should dial the relay.
func (r Relay) Enabled() bool { return strings.TrimSpace(r.URL) != "" }

func (r Relay) validate() error {
	raw := strings.TrimSpace(r.URL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("config: relay.url %q is not a url", r.URL)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		// Named rather than coerced. An https:// here is somebody assuming the
		// relay is an HTTP API, and silently rewriting the scheme would hide
		// that the two are different protocols with different failure modes.
		return fmt.Errorf("config: relay.url is %s://, and the relay speaks ws:// or wss://", u.Scheme)
	}
	if u.Scheme == "ws" && !isLoopbackHost(u.Hostname()) {
		// The relay carries pairing traffic and sealed frames, and neither is
		// harmed by a passive observer — but an unencrypted hop to a public host
		// leaks who is talking to whom, which is the metadata §7 is otherwise
		// careful about. Loopback is exempted so a test or a local relay works.
		return fmt.Errorf("config: relay.url is ws:// to %s; use wss:// off the local machine", u.Hostname())
	}
	if r.MaxStreams < 0 {
		return fmt.Errorf("config: relay.max_streams is %d", r.MaxStreams)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// Connectors is ORCHESTRATOR.md §4b's missing half: what *could* be connected
// on this machine.
//
// connector.Proposer only ever proposes something in its set, and the set is
// built from this section. That is the narrower half of §4b and it is worth
// saying out loud rather than glossing: §4b's example is Relay noticing a
// printer it had never heard of, and this only lets Relay notice a printer the
// user has already told it about. The wider version needs a catalogue of
// descriptors that can be proposed before they can be reached, plus a setup
// flow on accept — and until that exists, granting a connector with no tools
// behind it would be a decision with nothing on the other end.
//
// It is a named table rather than map[string]any because a generic map would
// advertise support for connectors that do not exist in this build. There is
// exactly one Connector implementation in the tree.
type Connectors struct {
	Prusa PrusaConnector `toml:"prusa"`

	// Window is how far back evidence counts, as a Go duration ("168h").
	// Empty takes connector.DefaultWindow.
	Window string `toml:"window"`
	// Cooldown is how long a dismissed proposal stays dismissed. Empty takes
	// connector.DefaultCooldown. A proposal the user said no to is not one to
	// make again next week: repeated asking is how blind-accept is trained.
	Cooldown string `toml:"cooldown"`
	// MinEpisodes is how many separate conversations must mention something.
	// Zero takes connector.DefaultMinEpisodes — it never means "none", because
	// "a proposal needs evidence" is the first property the proposer enforces.
	MinEpisodes int `toml:"min_episodes"`
}

// PrusaConnector is one printer on the LAN.
type PrusaConnector struct {
	// Address is PrusaLink's base URL: "http://prusa.local".
	Address string `toml:"address"`
	// Credential is a REFERENCE to the PrusaLink API key — "env:PRUSA_KEY",
	// "vault:<id>" — and never the key itself, for the same reason every other
	// credential in this file is a reference.
	Credential string `toml:"credential"`
	// Storage is which of the printer's storages to read: "usb" or "local".
	Storage string `toml:"storage"`
}

// Configured reports whether a Prusa can actually be built.
//
// Both halves are required, and the credential is not optional the way the
// embedder's is. A connector whose tools would fail at call time is access we
// cannot deliver, so proposing it would be offering something that does not
// work — and connector.Set has no way to drop a tool that merely errors.
func (p PrusaConnector) Configured() bool {
	return strings.TrimSpace(p.Address) != "" && strings.TrimSpace(p.Credential) != ""
}

// Any reports whether any connector is configured at all.
func (c Connectors) Any() bool { return c.Prusa.Configured() }

// WindowDuration and CooldownDuration are the parsed forms. Zero means "take
// the connector package's default", which is where the numbers and their
// reasoning live — config does not import internal/connector and must not, so
// it carries the strings and not the constants.
func (c Connectors) WindowDuration() time.Duration   { return parseDur(c.Window) }
func (c Connectors) CooldownDuration() time.Duration { return parseDur(c.Cooldown) }

func parseDur(s string) time.Duration {
	if s == "" {
		return 0
	}
	// Validate already refused anything unparseable, so an error here can only
	// mean somebody built a Config by hand. Zero is the same answer as absent.
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// validate catches the connector mistakes that would otherwise surface as a
// proposal nobody can accept, or as no proposals at all.
func (c Connectors) validate() error {
	if err := checkRef("connectors.prusa.credential", c.Prusa.Credential); err != nil {
		return err
	}
	if a := strings.TrimSpace(c.Prusa.Address); a != "" {
		u, err := url.Parse(a)
		if err != nil {
			return fmt.Errorf("config: connectors.prusa.address is not a URL: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf(
				"config: connectors.prusa.address is %q; PrusaLink is an HTTP API on your "+
					"network, so this wants a full URL like http://prusa.local", a)
		}
	}
	if c.Prusa.Address != "" && c.Prusa.Credential == "" {
		return errors.New("config: connectors.prusa has an address and no credential; " +
			"PrusaLink refuses every request without its API key, so this connector " +
			"would be offered and then not work")
	}
	if c.MinEpisodes < 0 {
		return fmt.Errorf("config: connectors.min_episodes is %d; evidence cannot be negative",
			c.MinEpisodes)
	}
	for name, d := range map[string]string{
		"connectors.window":   c.Window,
		"connectors.cooldown": c.Cooldown,
	} {
		if d == "" {
			continue
		}
		v, err := time.ParseDuration(d)
		if err != nil {
			return fmt.Errorf("config: %s is not a duration (try \"168h\"): %w", name, err)
		}
		if v <= 0 {
			return fmt.Errorf("config: %s is %s; a window that has already closed proposes nothing", name, d)
		}
	}
	return nil
}

// Routing is MEMORY.md §8's half of the config: what the user already pays for.
//
// It is here rather than in a database table because it is a declaration and
// not an observation. Nothing probes for it, nothing infers it, and a fact the
// user typed belongs in the file they can read — the same argument the model
// and voice choices already make.
type Routing struct {
	// Entitlements are ids from [KnownEntitlements]: the subscriptions and
	// plans the user says they hold.
	//
	// Empty is the honest default and it is NOT the same as "api-keys". An
	// empty set means nobody has said, so routing skips the entitlement step
	// with a note; "api-keys" means the user said they hold no subscription,
	// which licenses picking a runtime on capability and load alone.
	Entitlements []string `toml:"entitlements"`
}

// KnownEntitlements mirrors routing.Entitled(), duplicated for the same reason
// [EmbeddingDims] duplicates store.EmbeddingDims and knownRefPrefixes
// duplicates llm.RefKind: config is a leaf package that anything can depend on,
// and routing -> llm -> config is a real import edge, so importing routing here
// would be a cycle. The two lists are pinned together by a test in
// entitlement_test.go, which is package config_test and may import both.
//
// The order is routing.Table's order and the test checks it, because that order
// is the preference order the router consults rows in.
var KnownEntitlements = []string{
	"claude-subscription",
	"chatgpt-subscription",
	"github-copilot",
	"zai-coding-plan",
	"minimax-coding-plan",
	"qwen-coding-plan",
	"kimi-coding-plan",
	"api-keys",
}

// KnownEntitlement reports whether an id is one the router will act on.
func KnownEntitlement(id string) bool {
	for _, k := range KnownEntitlements {
		if k == id {
			return true
		}
	}
	return false
}

// Log configures the structured logger.
type Log struct {
	Level  string `toml:"level"`  // debug | info | warn | error
	Format string `toml:"format"` // text | json
}

// Model is one of ORCHESTRATOR.md §2b's two models.
type Model struct {
	Vendor string `toml:"vendor"`
	Model  string `toml:"model"`
	// API is "openai" or "anthropic"; empty takes the vendor's default.
	API string `toml:"api"`
	// BaseURL overrides the vendor's; required for a custom provider.
	BaseURL string `toml:"base_url"`
	// Credential is a REFERENCE — "env:OPENROUTER_API_KEY", "file:...",
	// "exec:op read ...", "vault:<id>". Never a pasted secret: this file is
	// world-readable in every configuration where somebody has cat'd it into a
	// support ticket.
	Credential string `toml:"credential"`
}

// Models is the two-model split. The small one speaks; the big one works.
type Models struct {
	Small Model `toml:"small"`
	Big   Model `toml:"big"`
}

// Voice is ORCHESTRATOR.md §2a's speech choice: a primary provider plus an
// automatic fallback, so skipping the step produces a device that talks.
type Voice struct {
	Provider   string `toml:"provider"`
	Model      string `toml:"model"`
	Credential string `toml:"credential"`
	// Fallback is the keyless default. It is never empty: "mute out of the box"
	// is the worst possible first hour for a voice product.
	Fallback string `toml:"fallback"`
}

// EmbeddingDims mirrors store.EmbeddingDims, duplicated for the same reason
// knownRefPrefixes duplicates llm.RefKind: config stays a leaf package that
// anything can depend on. The two are pinned together by a test.
//
// It is a constant and not a setting because summary_vec is a vec0 table and a
// vec0 column's width is fixed when the table is created. Changing this number
// is a migration and a full re-embed, never an edit to this file.
const EmbeddingDims = 768

// Embedding providers.
//
// One field carries both the local/hosted distinction and, when hosted, which
// vendor — because every hosted embedder is OpenAI-compatible, so the vendor id
// already determines the wire shape. A second field would only be a second way
// to say the same thing and a second way to disagree with it.
const (
	// EmbedProviderOllama is the local runtime, and the recommended default on
	// the self-hosted tier. Any other non-empty value is a vendor id from
	// llm.Vendors() speaking OpenAI-compatible /v1/embeddings.
	EmbedProviderOllama = "ollama"
	// EmbedProviderNone switches embedding off, and so does an empty provider.
	// Search then runs lexical-only and says so on every query, which is a
	// supported state and not a broken one.
	EmbedProviderNone = "none"
)

// DefaultEmbedModel is the recommended local model: 768 dimensions natively,
// which is EmbeddingDims and MEMORY.md §3's sizing exactly.
const DefaultEmbedModel = "nomic-embed-text"

// Embedding is ORCHESTRATOR.md §2c's embedding choice — a third peer to
// [Models] and [Voice], not a special case.
//
// It is a step rather than a setting because the vector width is fixed at
// index-creation time: the choice has to be made before the backfill runs, and
// changing it later means re-embedding everything.
//
// **The recommendation inverts §2a and §2b, and that is deliberate rather than
// an inconsistency.** For voice and for the two orchestrator models the hosted
// option is recommended, because hosted is better and the data leaving the
// machine is one utterance at a time. Here the data is a summary of every
// session the user has ever run — MEMORY.md §3's ~22,000 of them — and
// MEMORY.md §6 and CLOUD.md §1 already make exactly this argument about the
// vault: self-hosting means it never leaves your hardware. It is not a cost
// argument; ~4–5 million tokens is well under a dollar once, for the whole
// 3.6 GB corpus. It is that the local model is the only option where the
// summaries stay put, and it costs minutes against the hour or two MEMORY.md §4
// budgets for summarisation. On Relay Cloud the box is ours, the model is
// preinstalled, and the step is informational.
type Embedding struct {
	// Provider is "ollama" for the local runtime, a vendor id from
	// llm.Vendors() for a hosted one, or empty (or "none") for no embedder.
	Provider string `toml:"provider"`
	// Model is the embedding model id.
	Model string `toml:"model"`
	// Dims is the width the probe measured, not the width a model card
	// promised. It is written down because the index is built to it, and
	// because comparing a measured number against the index's own width is how
	// a silently-swapped model gets caught later.
	Dims int `toml:"dims"`
	// BaseURL is the local runtime's host, or a hosted provider's endpoint.
	// Empty takes the vendor's catalog entry, or the local runtime's default.
	BaseURL string `toml:"base_url"`
	// Credential is a REFERENCE, never a pasted secret — and it is legitimately
	// EMPTY for the local runtime, which has no credential at all. That is a
	// normal state here rather than a misconfiguration to warn about, and it is
	// most of the point of the local runtime.
	Credential string `toml:"credential"`
}

// Configured reports whether an embedder should be built at all. When it is
// false the orchestrator passes a nil embedder to search, which degrades to
// lexical-only and reports it on every query. A box whose embedder is down
// should get worse search, not no search.
func (e Embedding) Configured() bool {
	return e.Provider != EmbedProviderNone && e.Provider != "" && e.Model != ""
}

// Local reports whether the vectors are computed on this machine.
func (e Embedding) Local() bool { return e.Provider == EmbedProviderOllama }

// RuntimeConfig is per-agent-runtime settings.
type RuntimeConfig struct {
	Enabled bool   `toml:"enabled"`
	Command string `toml:"command"`
	// StateDir is for runtimes whose state directory moves. Never hardcode
	// ~/.openclaw: OPENCLAW_STATE_DIR, --profile and --dev all relocate it, and
	// a reader that assumes the default silently reports an empty history as
	// success.
	StateDir string `toml:"state_dir"`
}

// Default is the configuration a fresh install starts from.
func Default() Config {
	return Config{
		Listen: DefaultListen,
		Log:    Log{Level: "info", Format: "text"},
		Models: Models{
			Small: Model{Vendor: "openrouter", Model: "openai/gpt-5.6-luna"},
			Big:   Model{Vendor: "openrouter", Model: "anthropic/opus-5"},
		},
		// Both ids come from the catalog rather than being restated here. They
		// were restated, and the fallback drifted to "edge-tts" against a catalog
		// that calls it "edge" — so `relay doctor` on an untouched config reported
		// the keyless voice as "no such voice option". That is the one row whose
		// entire purpose is that a user who skips the voice step still has a
		// device that talks, and SYSTEM.md §7c calls mute-out-of-the-box the worst
		// possible first hour for a voice product. Reading the catalog makes the
		// drift impossible rather than merely tested for.
		Voice: Voice{
			Provider: voice.Recommended().ID,
			Model:    voice.Recommended().DefaultModel,
			Fallback: voice.Fallback().ID,
		},
		// Local, and no credential, because the summaries of somebody's whole
		// working history are the thing this design is protecting.
		Embedding: Embedding{
			Provider: EmbedProviderOllama,
			Model:    DefaultEmbedModel,
			Dims:     EmbeddingDims,
		},
		Runtimes: map[string]RuntimeConfig{},
	}
}

// ConfigDir is where config.toml lives.
func ConfigDir() (string, error) {
	if v := os.Getenv(EnvConfigDir); v != "" {
		return v, nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Relay"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "relay"), nil
}

// DataDir is where the databases live.
func DataDir() (string, error) {
	if v := os.Getenv(EnvDataDir); v != "" {
		return v, nil
	}
	if runtime.GOOS == "darwin" {
		return ConfigDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "relay"), nil
	}
	return filepath.Join(home, ".local", "share", "relay"), nil
}

// Path is the full path to config.toml.
func Path() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// DBPath is the main database. SYSTEM.md §8: one file, and backup is a file
// copy.
func (c Config) DBPath() (string, error) { return c.dataPath("relay.db") }

// VaultPath is the credential vault — a different file, never indexed.
func (c Config) VaultPath() (string, error) { return c.dataPath("vault.db") }

// AuditPath is the credential and connector mutation log.
//
// A plain append-only file rather than a table in the main database, because
// DASHBOARD.md §4 wants evidence that survives anything happening to the thing
// being audited. A log stored inside the system it watches is worth less.
func (c Config) AuditPath() (string, error) { return c.dataPath("audit.log") }

func (c Config) dataPath(name string) (string, error) {
	dir := c.DataDir
	if dir == "" {
		var err error
		dir, err = DataDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, name), nil
}

// Load reads a config file, filling anything absent from Default. A missing
// file is not an error: a fresh box runs on defaults until the installer writes
// one.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, cfg.Validate()
}

// LoadDefault loads from the platform's config path.
func LoadDefault() (Config, error) {
	p, err := Path()
	if err != nil {
		return Default(), err
	}
	return Load(p)
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.Log.Level == "" {
		c.Log.Level = d.Log.Level
	}
	if c.Log.Format == "" {
		c.Log.Format = d.Log.Format
	}
	if c.Voice.Fallback == "" {
		c.Voice.Fallback = d.Voice.Fallback
	}
	// An empty provider is NOT filled in from the default. [Load] starts from
	// Default(), so a file with no [embedding] table already carries the
	// recommendation; a file that writes `provider = ""` is somebody saying they
	// want no embedder, and resurrecting it would make relayd reach for a
	// service on this machine that nobody asked it to reach for.
	if c.Embedding.Configured() && c.Embedding.Dims == 0 {
		c.Embedding.Dims = d.Embedding.Dims
	}
	if c.Embedding.Provider == EmbedProviderOllama && c.Embedding.Model == "" {
		c.Embedding.Model = d.Embedding.Model
	}
	if c.Runtimes == nil {
		c.Runtimes = map[string]RuntimeConfig{}
	}
}

// Validate catches the mistakes that would otherwise surface as silence.
func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: listen is empty")
	}
	for name, m := range map[string]Model{"small": c.Models.Small, "big": c.Models.Big} {
		if m.Vendor == "custom" && m.BaseURL == "" {
			return fmt.Errorf("config: models.%s uses the custom provider but has no base_url", name)
		}
	}
	if err := c.Embedding.validate(); err != nil {
		return err
	}
	if err := c.Routing.validate(); err != nil {
		return err
	}
	if err := c.Connectors.validate(); err != nil {
		return err
	}
	if err := c.Relay.validate(); err != nil {
		return err
	}
	// A pasted secret is the one config mistake worth refusing outright: it
	// ends up in a backup, a screenshot and a support ticket. References only.
	for name, m := range map[string]string{
		"models.small.credential": c.Models.Small.Credential,
		"models.big.credential":   c.Models.Big.Credential,
		"voice.credential":        c.Voice.Credential,
		"embedding.credential":    c.Embedding.Credential,
	} {
		if err := checkRef(name, m); err != nil {
			return err
		}
	}
	return nil
}

// validate catches the embedding mistakes that would otherwise surface as an
// index nobody can query.
//
// The width check is the important one, and it is here as well as at the probe
// because the two catch different things: the probe catches a model that lies
// about its width, and this catches a config file edited by hand into a state
// where the index and the embedder disagree. Both name both numbers, because
// "dimension mismatch" without the numbers tells nobody anything.
func (e Embedding) validate() error {
	if e.Provider == "" || e.Provider == EmbedProviderNone {
		// Switching embedding off is allowed and is not a half-configuration:
		// search degrades to lexical and reports it. Nothing else to check.
		return nil
	}
	if e.Model == "" {
		return fmt.Errorf("config: embedding.provider is %q but no model is named", e.Provider)
	}
	if e.Dims != 0 && e.Dims != EmbeddingDims {
		return fmt.Errorf(
			"config: embedding.dims is %d and the index is %d wide. A vec0 column's width is "+
				"fixed when the table is created, so this cannot be changed in a config file — "+
				"choose a %d-dimension model, or re-embed into a new index",
			e.Dims, EmbeddingDims, EmbeddingDims)
	}
	// The vendor id is not checked against the catalog here: config is a leaf
	// package and importing llm to validate a string would invert that for no
	// gain. An unknown vendor with no base URL fails at construction, which is
	// before any call goes out. The one case that is checkable without the
	// catalog is checked, because it is the one people hit.
	if e.Provider == "custom" && e.BaseURL == "" {
		return errors.New("config: embedding uses the custom provider but has no base_url")
	}
	return nil
}

// validate refuses an entitlement id the router would not recognise.
//
// Refusing rather than dropping is the deliberate choice, and it costs
// something: a config written by a newer relay will not load on an older
// relayd. The alternative costs more. An entitlement is a billing fact that
// overrides capability comparison, so a typo that is silently ignored is
// exactly the failure MEMORY.md §8 exists to prevent — the user believes their
// subscription is being used and it is not, and nothing anywhere says so. The
// message names the bad id and prints the whole valid list, because a rejection
// that does not tell you what to type instead is a dead end.
func (r Routing) validate() error {
	for _, e := range r.Entitlements {
		if !KnownEntitlement(e) {
			return fmt.Errorf(
				"config: routing.entitlements has %q, which is not an entitlement this "+
					"build knows. Valid ids: %s", e, strings.Join(KnownEntitlements, ", "))
		}
	}
	return nil
}

// knownRefPrefixes mirrors llm.RefKind. It is duplicated rather than imported
// so config stays a leaf package that anything can depend on.
var knownRefPrefixes = []string{"env:", "file:", "exec:", "vault:", "inline:"}

func checkRef(field, ref string) error {
	if ref == "" {
		return nil
	}
	for _, p := range knownRefPrefixes {
		if len(ref) > len(p) && ref[:len(p)] == p {
			return nil
		}
	}
	// A bare string is read as an env var name, which is what people type.
	// Anything with a colon in it and an unknown prefix is almost certainly a
	// pasted secret.
	for i := range ref {
		if ref[i] == ':' {
			return fmt.Errorf("config: %s looks like a pasted secret; use env:, file:, exec: or vault: instead", field)
		}
	}
	return nil
}

// Save writes the config file, creating its directory with 0700.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}
