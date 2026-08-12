package apps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The end-to-end tests. Every one of these starts a real Node process under the
// real sandbox: a containment story exercised only through a double is a
// containment story nobody has tested.

// probeApp reports what it can see, so a test can assert on the *app's* view
// rather than on the host's idea of it. That distinction is the whole minting
// rule: "memory" in ctx has to be false, not "memory.search throws".
const probeApp = `
export default {
  async onTrigger(ctx) {
    const probe = {
      trigger: ctx.trigger.type,
      granted: ctx.granted,
      declined: ctx.declined,
      keys: Object.keys(ctx).sort(),
      hasMemory: "memory" in ctx,
      hasGlasses: "glasses" in ctx,
      hasAgent: "agent" in ctx,
      hasFetch: "fetch" in ctx,
      hasSay: "say" in ctx,
      hasStorage: "storage" in ctx,
      memoryKeys: ctx.memory ? Object.keys(ctx.memory).sort() : [],
    };
    ctx.log(JSON.stringify(probe));
  },
};
`

type probeResult struct {
	Trigger    string   `json:"trigger"`
	Granted    []string `json:"granted"`
	Declined   []string `json:"declined"`
	Keys       []string `json:"keys"`
	HasMemory  bool     `json:"hasMemory"`
	HasGlasses bool     `json:"hasGlasses"`
	HasAgent   bool     `json:"hasAgent"`
	HasFetch   bool     `json:"hasFetch"`
	HasSay     bool     `json:"hasSay"`
	HasStorage bool     `json:"hasStorage"`
	MemoryKeys []string `json:"memoryKeys"`
}

func manifestWith(id string, perms string, triggers string, extra string) string {
	return `{
  "id": "` + id + `",
  "name": "Probe",
  "version": "1.0.0",
  "description": "An app that reports what it was given.",
  "author": { "name": "Test", "url": "https://example.com" },
  "permissions": [` + perms + `],
  "triggers": [` + triggers + `]` + extra + `
}`
}

func TestAnUngrantedCapabilityIsAbsentRatherThanRefusing(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.probe",
		`{"scope":"memory.read","reason":"To find the meeting you just left."}`,
		`{"type":"phrase","match":"probe"}`, ""), probeApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase, Transcript: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s (logs %q)", inv.Outcome, inv.Error, tr.logged())
	}

	var p probeResult
	probeJSON(t, tr.logged(), &p)

	if !p.HasMemory {
		t.Error("memory.read was granted, so ctx.memory must exist")
	}
	// The point of the whole design: not present-and-refusing, absent.
	for _, absent := range []struct {
		name string
		got  bool
	}{
		{"glasses", p.HasGlasses}, {"agent", p.HasAgent}, {"fetch", p.HasFetch}, {"say", p.HasSay},
	} {
		if absent.got {
			t.Errorf("ctx.%s exists but no scope paid for it", absent.name)
		}
	}
	if !p.HasStorage {
		t.Error("storage needs no scope and must always be there")
	}
	// memory.read alone must not mint memory.write.
	for _, k := range p.MemoryKeys {
		if k == "write" {
			t.Error("memory.write was minted from memory.read alone")
		}
	}
	want := []string{"extractCommitments", "get", "recentEpisode", "search"}
	if strings.Join(p.MemoryKeys, ",") != strings.Join(want, ",") {
		t.Errorf("memory members = %v, want %v", p.MemoryKeys, want)
	}
}

func TestDecliningAScopeRemovesItFromTheObject(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.declined",
		`{"scope":"memory.read","reason":"To find the meeting you just left."},`+
			`{"scope":"glasses.speaker","reason":"To read the summary back to you."}`,
		`{"type":"phrase","match":"probe"}`, ""), probeApp)

	// The user accepted memory and declined the speaker.
	inst := tr.install(t, src, ScopeMemoryRead)

	if _, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase}); err != nil {
		t.Fatal(err)
	}
	var p probeResult
	probeJSON(t, tr.logged(), &p)

	if p.HasGlasses || p.HasSay {
		t.Error("the speaker was declined, so neither ctx.glasses nor ctx.say may exist")
	}
	if len(p.Declined) != 1 || p.Declined[0] != string(ScopeGlassesSpeaker) {
		t.Errorf("declined = %v, want [glasses.speaker] so the app can say what it is missing", p.Declined)
	}
}

func TestConsentCannotGrantWhatTheSheetDidNotShow(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.overreach",
		`{"scope":"memory.read","reason":"To find the meeting you just left."}`,
		`{"type":"phrase","match":"probe"}`, ""), probeApp)
	m, err := ReadManifest(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Install(m, Consent{Granted: []Scope{ScopeMemoryRead, ScopeGlassesCamera}}, src,
		InstallOptions{Dir: filepath.Join(tr.Dir, "apps")})
	if !errors.Is(err, ErrNotRequested) {
		t.Fatalf("granting an unrequested scope must fail, got %v", err)
	}
}

const memoryApp = `
export default {
  async onTrigger(ctx) {
    const ep = await ctx.memory.recentEpisode({ kind: "meeting", within: 3600000 });
    if (!ep) { await ctx.say("no meeting"); return; }
    const commitments = await ctx.memory.extractCommitments(ep);
    const summary = await ctx.agent.ask("Summarise:\n" + ep.transcript);
    const { id } = await ctx.memory.write({
      kind: "note", title: "Standup — " + ep.startedAt.toISOString().slice(0, 10),
      body: summary, commitments,
    });
    ctx.log("wrote " + id);
    await ctx.say("Saved. " + commitments.length + " commitment.");
  },
};
`

func TestAnAppReadsMemoryAndEveryReadIsRecorded(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.standup",
		`{"scope":"memory.read","reason":"To read the meeting you just left."},`+
			`{"scope":"memory.write","reason":"To save the notes it extracts."},`+
			`{"scope":"agent.session","reason":"To summarise using your own model."},`+
			`{"scope":"glasses.speaker","reason":"To read the commitments back to you."}`,
		`{"type":"phrase","match":"wrap up the standup"}`, ""), memoryApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase, Transcript: "wrap up the standup"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s (logs %q)", inv.Outcome, inv.Error, tr.logged())
	}

	spoken := tr.Device.Spoken()
	if len(spoken) != 1 || !strings.Contains(spoken[0], "1 commitment") {
		t.Errorf("spoken = %v", spoken)
	}
	if len(inv.Spoken) != 1 {
		t.Errorf("the invocation record must carry what was said: %v", inv.Spoken)
	}
	if len(tr.Sink.Notes) != 1 || tr.Sink.Notes[0].Body != "a summary" {
		t.Errorf("notes = %+v", tr.Sink.Notes)
	}

	// §5: "the user can see exactly which app touched which episode".
	accesses := tr.Access.All()
	if len(accesses) < 3 {
		t.Fatalf("expected recentEpisode, get, extract and write to be recorded, got %d: %+v",
			len(accesses), accesses)
	}
	byOp := map[string]Access{}
	for _, a := range accesses {
		byOp[a.Op] = a
		if a.AppID != "dev.test.standup" {
			t.Errorf("access recorded against %q", a.AppID)
		}
		if a.Invocation != inv.ID {
			t.Errorf("access %s is not tied to the invocation that caused it", a.Op)
		}
	}
	for _, want := range []string{
		string(MethodMemoryRecentEpisode), string(MethodMemoryExtract), string(MethodMemoryWrite),
	} {
		if _, ok := byOp[want]; !ok {
			t.Errorf("%s was not recorded", want)
		}
	}
	if got := byOp[string(MethodMemoryRecentEpisode)].Episodes; len(got) != 1 || got[0] != "ep-1" {
		t.Errorf("the access log must name the episode read, got %v", got)
	}
}

const hangingApp = `
export default {
  async onTrigger(ctx) {
    ctx.log("about to hang");
    await new Promise(() => {});
  },
};
`

func TestAnAppThatHangsIsKilled(t *testing.T) {
	tr := newTestRuntime(t, func(o *Options) {
		o.Limits.WallClock = 1500 * time.Millisecond
		o.Limits.Grace = 200 * time.Millisecond
	})
	src := writeApp(t, manifestWith("dev.test.hang", ``,
		`{"type":"phrase","match":"hang"}`, `, "timeoutMs": 1500`), hangingApp)
	inst := tr.install(t, src)

	began := time.Now()
	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %s (%s); a hanging app must be killed", inv.Outcome, inv.Error)
	}
	if elapsed := time.Since(began); elapsed > 8*time.Second {
		t.Errorf("kill took %s, which is not a kill", elapsed)
	}
	if !strings.Contains(inv.Error, "still running") {
		t.Errorf("the record must say why: %q", inv.Error)
	}
}

const throwingApp = `
export default {
  onTrigger() { throw new Error("the app decided this was hopeless"); },
};
`

func TestAnAppThatThrowsIsRecordedAsFailedNotCrashed(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.throws", ``, `{"type":"phrase","match":"throw"}`, ""), throwingApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", inv.Outcome)
	}
	if !strings.Contains(inv.Error, "hopeless") {
		t.Errorf("the app's own message must survive: %q", inv.Error)
	}
}

// escapeApp tries every route out of the sandbox that a malicious package would
// try first. None of them are exotic; that is the point.
//
// The port is compiled into the source rather than passed in the environment,
// because the environment is one of the things being tested: [Runtime] builds it
// from nothing, so an app cannot read anything of relayd's out of `process.env`
// — including, here, the address the test wants it to try.
func escapeApp(port int) string {
	return `
export default {
  async onTrigger(ctx) {
    const out = {};
    try { const fs = await import("node:fs"); fs.readFileSync("/etc/hostname"); out.readEtc = "ALLOWED"; }
    catch (e) { out.readEtc = e.code || e.message; }
    try { const fs = await import("node:fs"); fs.writeFileSync(process.env.HOME + "/../root/pwned", "x"); out.writeRoot = "ALLOWED"; }
    catch (e) { out.writeRoot = e.code || e.message; }
    try { const cp = await import("node:child_process"); cp.execSync("id"); out.spawn = "ALLOWED"; }
    catch (e) { out.spawn = e.code || e.message; }
    try { const { Worker } = await import("node:worker_threads"); new Worker("1", { eval: true }); out.worker = "ALLOWED"; }
    catch (e) { out.worker = e.code || e.message; }
    out.envKeys = Object.keys(process.env).sort();
    try {
      const net = await import("node:net");
      out.socket = await new Promise((resolve) => {
        const s = net.connect({ host: "127.0.0.1", port: ` + itoa(port) + ` });
        s.setTimeout(2000);
        s.on("connect", () => { s.destroy(); resolve("CONNECTED"); });
        s.on("timeout", () => { s.destroy(); resolve("TIMEOUT"); });
        s.on("error", (e) => resolve("ERR:" + (e.code || e.message)));
      });
    } catch (e) { out.socket = "throw:" + (e.code || e.message); }
    ctx.log(JSON.stringify(out));
  },
};
`
}

type escapeResult struct {
	ReadEtc   string   `json:"readEtc"`
	WriteRoot string   `json:"writeRoot"`
	Spawn     string   `json:"spawn"`
	Worker    string   `json:"worker"`
	Socket    string   `json:"socket"`
	EnvKeys   []string `json:"envKeys"`
}

func TestTheSandboxRefusesTheObviousEscapes(t *testing.T) {
	// A listener the app will try to reach. If the sandbox gave it a network,
	// this connects — which is exactly what the assertion below is for.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	tr := newTestRuntime(t)
	src := writeApp(t, manifestWith("dev.test.escape", ``, `{"type":"phrase","match":"escape"}`, ""),
		escapeApp(port))
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s (%q)", inv.Outcome, inv.Error, tr.logged())
	}

	var e escapeResult
	probeJSON(t, tr.logged(), &e)

	if e.ReadEtc == "ALLOWED" {
		t.Error("the app read /etc/hostname; the filesystem boundary is not holding")
	}
	if e.WriteRoot == "ALLOWED" {
		t.Error("the app wrote outside its scratch")
	}
	if e.Spawn == "ALLOWED" {
		t.Error("the app spawned a process")
	}
	if e.Worker == "ALLOWED" {
		t.Error("the app started a worker thread")
	}

	// The environment is built from nothing, so relayd's own variables — API
	// keys, database paths — are not reachable through process.env.
	want := map[string]bool{"HOME": true, "TMPDIR": true, "NODE_ENV": true, "RELAY_APP_ID": true}
	for _, k := range e.EnvKeys {
		if !want[k] {
			t.Errorf("the app can see %s in its environment; the env is supposed to be built, not inherited", k)
		}
	}

	// The network assertion is conditional on what the sandbox actually got,
	// because asserting a boundary this machine could not give us would be
	// asserting something about the CI runner rather than about the code. The
	// listener is real and reachable from this process, so a sandbox that
	// reports "enforced" and lets the app connect fails here.
	if c, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 2*time.Second); err != nil {
		t.Fatalf("the probe listener is not reachable from the test itself, so the assertion below "+
			"would prove nothing: %v", err)
	} else {
		c.Close()
	}
	switch tr.Enforcement().Network.Control {
	case ControlEnforced:
		if e.Socket == "CONNECTED" {
			t.Errorf("network isolation reports enforced and the app connected anyway: %q", e.Socket)
		}
		if !strings.HasPrefix(e.Socket, "ERR:") {
			t.Errorf("an empty network namespace should fail the connect outright, got %q", e.Socket)
		}
	default:
		t.Logf("network isolation is %s here (%s); socket probe said %q",
			tr.Enforcement().Network.Control, tr.SandboxName(), e.Socket)
	}
}

const fetchApp = `
export default {
  async onTrigger(ctx) {
    const out = {};
    try { const r = await ctx.fetch("https://allowed.example/thing"); out.allowed = r.status; }
    catch (e) { out.allowed = "ERR:" + e.code + ":" + e.message; }
    try { const r = await ctx.fetch("https://elsewhere.example/exfil"); out.denied = r.status; }
    catch (e) { out.denied = e.code; }
    ctx.log(JSON.stringify(out));
  },
};
`

func TestEgressIsAllowlistedAndBothOutcomesAreRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	// The allowlist names hosts; the test client rewrites them onto the local
	// server, so the guard is doing the deciding and no packet leaves the box.
	client := &http.Client{Transport: rewriteTo(upstream.Listener.Addr().String())}

	tr := newTestRuntime(t, func(o *Options) { o.FetchClient = client })
	src := writeApp(t, manifestWith("dev.test.fetch",
		`{"scope":"net.fetch","reason":"To post the summary to your own wiki."}`,
		`{"type":"phrase","match":"fetch"}`, `, "allowedHosts": ["allowed.example"]`), fetchApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s (%q)", inv.Outcome, inv.Error, tr.logged())
	}

	var got struct {
		Allowed any    `json:"allowed"`
		Denied  string `json:"denied"`
	}
	probeJSON(t, tr.logged(), &got)
	if f, ok := got.Allowed.(float64); !ok || int(f) != 204 {
		t.Errorf("the allowlisted host should have answered 204, got %v", got.Allowed)
	}
	if got.Denied != CodeDenied {
		t.Errorf("the host that is not on the allowlist must be refused with %q, got %q", CodeDenied, got.Denied)
	}

	attempts := tr.Egress.All()
	if len(attempts) != 2 {
		t.Fatalf("both the allowed and the denied attempt must be recorded, got %+v", attempts)
	}
	var sawDenied bool
	for _, a := range attempts {
		if !a.Allowed {
			sawDenied = true
			if !strings.Contains(a.Reason, "not on this app's allowlist") {
				t.Errorf("a denial has to say why: %q", a.Reason)
			}
			if strings.Contains(a.URL, "?") {
				t.Errorf("the egress log must not carry the query string: %q", a.URL)
			}
		}
	}
	if !sawDenied {
		t.Error("the denied attempt was not recorded, which is the half that matters")
	}
}

func TestAnAppThatReadsYourLifeDoesNotRunWithoutNetworkIsolation(t *testing.T) {
	tr := newTestRuntime(t, func(o *Options) { o.DisableSandbox = true })
	if tr.Enforcement().Network.Control == ControlEnforced {
		t.Fatal("the sandbox was disabled, so network isolation cannot be enforced")
	}

	src := writeApp(t, manifestWith("dev.test.reader",
		`{"scope":"memory.read","reason":"To find the meeting you just left."}`,
		`{"type":"phrase","match":"read"}`, ""), probeApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if !errors.Is(err, ErrCannotContain) {
		t.Fatalf("want ErrCannotContain, got %v", err)
	}
	if inv.Outcome != OutcomeRefused {
		t.Errorf("outcome = %s, want refused", inv.Outcome)
	}
	if !strings.Contains(err.Error(), "exfiltration") {
		t.Errorf("the refusal should say what it is about: %q", err)
	}

	// An app that cannot read anything is not an exfiltration risk, so it runs.
	harmless := writeApp(t, manifestWith("dev.test.harmless", ``, `{"type":"phrase","match":"x"}`, ""), probeApp)
	hi := tr.install(t, harmless)
	if _, err := tr.Invoke(context.Background(), hi, TriggerFrame{Type: TriggerPhrase}); err != nil {
		t.Fatalf("an app with no read scopes must still run: %v", err)
	}
}

func TestTheEnforcementReportNeverOverstates(t *testing.T) {
	tr := newTestRuntime(t)
	e := tr.Enforcement()

	for _, c := range e.Controls() {
		switch c.Control {
		case ControlEnforced, ControlPartial:
			if c.By == "" {
				t.Errorf("%s claims %s with no mechanism named", c.Name, c.Control)
			}
			if c.Note == "" {
				t.Errorf("%s claims %s with no note saying how far it goes", c.Name, c.Control)
			}
		case ControlDeclared:
			if c.By != "" {
				t.Errorf("%s is declared but names %q as enforcing it", c.Name, c.By)
			}
		default:
			t.Errorf("%s has an unknown control %q", c.Name, c.Control)
		}
	}

	// Memory is never reported as fully enforced, because it cannot be: V8's
	// heap cap and the kernel's address-space cap are different numbers.
	if e.Memory.Control == ControlEnforced {
		t.Error("the memory cap is two partial caps and must not be reported as one enforced one")
	}
	if !strings.Contains(e.Memory.Note, "heap") {
		t.Errorf("the memory note has to say which half is which: %q", e.Memory.Note)
	}
	if e.WallClock.Control != ControlEnforced {
		t.Error("the wall clock is the supervisor's and is always enforced")
	}
}

func TestScratchIsClearedBetweenInvocations(t *testing.T) {
	tr := newTestRuntime(t)
	const writer = `
export default {
  async onTrigger(ctx) {
    const fs = await import("node:fs");
    const p = process.env.HOME + "/left-behind";
    ctx.log("exists:" + fs.existsSync(p));
    fs.writeFileSync(p, "x");
  },
};
`
	src := writeApp(t, manifestWith("dev.test.scratch", ``, `{"type":"phrase","match":"s"}`, ""), writer)
	inst := tr.install(t, src)

	for i := 0; i < 2; i++ {
		inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
		if err != nil || inv.Outcome != OutcomeCompleted {
			t.Fatalf("run %d: %s %s %q", i, inv.Outcome, inv.Error, tr.logged())
		}
	}
	for _, l := range tr.logged() {
		if l == "exists:true" {
			t.Error("the scratch survived an invocation; it is scratch, not storage")
		}
	}
}

func TestStorageSurvivesBetweenInvocationsAndScratchDoesNot(t *testing.T) {
	tr := newTestRuntime(t)
	const counter = `
export default {
  async onTrigger(ctx) {
    const n = (await ctx.storage.get("runs")) ?? 0;
    await ctx.storage.set("runs", n + 1);
    ctx.log("runs=" + (n + 1));
  },
};
`
	src := writeApp(t, manifestWith("dev.test.storage", ``, `{"type":"phrase","match":"s"}`, ""), counter)
	inst := tr.install(t, src)
	for i := 0; i < 3; i++ {
		if _, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase}); err != nil {
			t.Fatal(err)
		}
	}
	logs := tr.logged()
	if len(logs) < 3 || logs[len(logs)-1] != "runs=3" {
		t.Errorf("storage did not persist across invocations: %v", logs)
	}

	// And it lives on relayd's side of the boundary, not in a directory the app
	// can walk to.
	entries, err := os.ReadDir(inst.Layout.Data)
	if err != nil || len(entries) == 0 {
		t.Fatalf("storage should be in the app's data directory: %v %v", entries, err)
	}
	if strings.HasPrefix(inst.Layout.Data, inst.Layout.Scratch) ||
		strings.HasPrefix(inst.Layout.Data, inst.Layout.Root) {
		t.Error("the data directory must not be inside anything the app can reach")
	}
}

func TestAppLogLinesGoThroughTheSecretDetector(t *testing.T) {
	tr := newTestRuntime(t)
	// A synthetic AWS-shaped key. Nothing here is real; the detector's ruleset
	// is `internal/index`'s and this is the shape it was measured against.
	const leaky = `
export default {
  onTrigger(ctx) {
    ctx.log("found a key: AKIA` + "IOSFODNN7EXAMPLE" + `");
    console.log("and on stdout: AKIA` + "IOSFODNN7EXAMPLE" + `");
  },
};
`
	src := writeApp(t, manifestWith("dev.test.leak", ``, `{"type":"phrase","match":"l"}`, ""), leaky)
	inst := tr.install(t, src)
	if _, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase}); err != nil {
		t.Fatal(err)
	}
	for _, l := range tr.Logs.All() {
		if strings.Contains(l.Message, "AKIA"+"IOSFODNN7EXAMPLE") {
			t.Errorf("a credential reached the log through %s: %q", l.Stream, l.Message)
		}
	}
	if len(tr.Logs.All()) < 2 {
		t.Errorf("both ctx.log and stdout should have been captured, got %d lines", len(tr.Logs.All()))
	}
}

func TestTheAppRootIsReadOnlyOnDisk(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, minimalManifest, probeApp)
	inst := tr.install(t, src)

	entry, err := os.Stat(inst.Entry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mode().Perm()&0o200 != 0 {
		t.Errorf("the app root should be read-only after install, entry mode is %v", entry.Mode())
	}
	// The mode bits are defence in depth and not the boundary — the boundary is
	// the sandbox, which never hands the app a writable handle on this tree — so
	// this half of the check only means anything for a process the bits apply
	// to. root ignores them, and asserting otherwise would be asserting
	// something about the test runner.
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(inst.Layout.Root, "new"), []byte("x"), 0o600); err == nil {
			t.Error("writing into a frozen app root should fail")
		}
	}
	if err := Uninstall(inst.Layout); err != nil {
		t.Errorf("uninstall has to be able to remove what install froze: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const memoryHogApp = `
export default {
  async onTrigger(ctx) {
    ctx.log("allocating");
    const held = [];
    for (;;) {
      // Plain JS objects, so this lands in the old space the cap actually
      // governs rather than in an external buffer it does not.
      const chunk = [];
      for (let i = 0; i < 200000; i++) chunk.push({ i, s: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" });
      held.push(chunk);
    }
  },
};
`

func TestAnAppThatEatsMemoryIsKilled(t *testing.T) {
	tr := newTestRuntime(t, func(o *Options) {
		o.Limits.Memory = 48 << 20
		o.Limits.WallClock = 25 * time.Second
	})
	src := writeApp(t, manifestWith("dev.test.hog", ``, `{"type":"phrase","match":"hog"}`,
		`, "timeoutMs": 25000`), memoryHogApp)
	inst := tr.install(t, src)

	began := time.Now()
	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	// V8 aborts the process on a heap OOM, so this is a crash and not a timeout
	// — and the difference is the point: the heap cap is what stopped it, not
	// the clock.
	if inv.Outcome != OutcomeCrashed {
		t.Fatalf("outcome = %s (%s) after %s; the heap cap did not bind",
			inv.Outcome, inv.Error, time.Since(began))
	}
	if time.Since(began) > 20*time.Second {
		t.Errorf("it ran to the wall clock rather than to the heap cap: %s", time.Since(began))
	}
}

const cpuHogApp = `
export default {
  onTrigger(ctx) {
    ctx.log("spinning");
    let x = 0;
    for (;;) { x = (x + 1) % 1000000007; }
  },
};
`

func TestAnAppThatSpinsHitsTheCPUCapBeforeTheClock(t *testing.T) {
	tr := newTestRuntime(t, func(o *Options) {
		o.Limits.CPUTime = time.Second
		o.Limits.WallClock = 30 * time.Second
	})
	if tr.Enforcement().CPU.Control != ControlEnforced {
		t.Skipf("this platform reports the CPU cap as %s, so there is nothing to assert",
			tr.Enforcement().CPU.Control)
	}
	src := writeApp(t, manifestWith("dev.test.spin", ``, `{"type":"phrase","match":"spin"}`,
		`, "timeoutMs": 30000`), cpuHogApp)
	inst := tr.install(t, src)

	began := time.Now()
	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(began)
	if inv.Outcome == OutcomeTimeout {
		t.Fatalf("the wall clock caught it after %s; the CPU cap should have, first", elapsed)
	}
	if inv.Outcome != OutcomeCrashed {
		t.Fatalf("outcome = %s (%s)", inv.Outcome, inv.Error)
	}
	if elapsed > 15*time.Second {
		t.Errorf("a one-second CPU cap took %s to bite", elapsed)
	}
}

func TestEveryInvocationCarriesWhatWasContainingIt(t *testing.T) {
	tr := newTestRuntime(t)
	src := writeApp(t, minimalManifest, probeApp)
	inst := tr.install(t, src)
	inv, err := tr.Invoke(context.Background(), inst, TriggerFrame{Type: TriggerPhrase})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Sandbox == "" {
		t.Error("the record must name the sandbox that was in force")
	}
	if inv.Enforcement.WallClock.Control != ControlEnforced {
		t.Error("the record must carry the enforcement, not a summary of it")
	}
	if inv.AccessLogDurable {
		t.Error("this runtime holds an in-memory access log and the record has to say so")
	}
	if inv.ID == "" || inv.EndedAt.Before(inv.StartedAt) {
		t.Errorf("record = %+v", inv)
	}
}
