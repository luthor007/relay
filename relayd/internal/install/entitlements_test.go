package install

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// MEMORY.md §8's entitlement table has been correct and tested since it was
// written, and it had never once fired, because nothing anywhere recorded an
// entitlement. These tests are about the half that was missing: the question,
// what it refuses to infer, and what an unattended run records.

// The fixture machine has Claude Code and Codex and nothing else, so exactly
// two of the four questions can matter. Asking about Copilot on a machine with
// neither OpenClaw nor OpenCode would record a fact that can never fire, and
// four questions where two apply is how an installer stops being read.
func TestEntitlementsAreOnlyAskedForRuntimesThatAreHere(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	asked := map[string]bool{}
	for _, id := range script.Asked {
		asked[id] = true
	}
	for _, want := range []string{"entitlements.claude", "entitlements.chatgpt"} {
		if !asked[want] {
			t.Errorf("the installer never asked %q on a machine that has that runtime", want)
		}
	}
	for _, unwanted := range []string{"entitlements.copilot", "entitlements.coding_plan"} {
		if asked[unwanted] {
			t.Errorf("the installer asked %q with no runtime here that could use it", unwanted)
		}
	}
}

// A machine with none of the five is asked nothing at all, and says so in the
// outcome rather than in the transcript.
//
// This is the shape that keeps the step from becoming a tax on every install.
// A user with no agent runtime yet has no routing decision to influence, and a
// section whose only content is that it does not apply is worse than silence.
func TestABareMachineIsAskedNoEntitlementQuestions(t *testing.T) {
	script := NewScript(map[string]string{})
	env, _, _ := fixtureEnv()
	// Strip the machine down: no binaries, no history.
	env.FS = &detect.MemFS{Dirs: []string{home}}
	env.Exec = &detect.FakeExec{}

	out, err := chooseEntitlements(context.Background(),
		Options{Prompt: script, Env: env}.withDefaults(),
		detect.Detect(context.Background(), env, detect.Options{SkipProcesses: true}),
		nil, ModelsOutcome{})
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if len(script.Asked) != 0 {
		t.Errorf("a bare machine was asked %v", script.Asked)
	}
	if len(out.Entitlements) != 0 {
		t.Errorf("a bare machine recorded %v", out.Entitlements)
	}
	if out.Skipped == "" {
		t.Error("nothing was asked and the outcome does not say why, so the summary shows a blank")
	}
}

// `relay setup --yes` must record nothing.
//
// Auto answers every confirmation with its default, and every default here is
// no — deliberately, because an entitlement OVERRIDES capability comparison.
// One invented by a script is a routing decision nobody made, sending work to a
// runtime the user may not be paying for at all.
func TestAnUnattendedInstallRecordsNoEntitlement(t *testing.T) {
	env, _, _ := fixtureEnv()
	auto := &Auto{}
	out, err := chooseEntitlements(context.Background(),
		Options{Prompt: auto, Env: env}.withDefaults(),
		detect.Detect(context.Background(), env, detect.Options{SkipProcesses: true}),
		nil, ModelsOutcome{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entitlements) != 0 {
		t.Fatalf("an unattended run recorded %v; --yes must not buy the user a subscription",
			out.Entitlements)
	}
	// It was asked, and answered no. Not asking at all would be a different
	// bug with the same symptom, and only one of them is fixed by a prompter.
	if len(out.Asked) == 0 {
		t.Error("the unattended run skipped the questions rather than defaulting them")
	}
}

// The whole point of the row: a recorded entitlement reaches the config file
// the daemon reads, by the id the routing table uses.
func TestARecordedEntitlementReachesTheWrittenConfig(t *testing.T) {
	answers := baseAnswers()
	answers["entitlements.claude"] = "yes"

	opts, script, fs := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if got := res.Config.Routing.Entitlements; len(got) != 1 || got[0] != "claude-subscription" {
		t.Fatalf("Result carries %v, want exactly the Claude row", got)
	}

	// And it survives the file, because the file is the only store there is:
	// no schema column, no console editor.
	var back config.Config
	if _, err := tomlDecode(fs.Files[opts.ConfigPath], &back); err != nil {
		t.Fatalf("the written config does not parse: %v", err)
	}
	if len(back.Routing.Entitlements) != 1 || back.Routing.Entitlements[0] != "claude-subscription" {
		t.Fatalf("[routing] in the file is %v", back.Routing.Entitlements)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("the installer wrote a config the daemon will refuse: %v", err)
	}
	if !strings.Contains(script.Output(), "claude-subscription") {
		t.Error("the installer recorded an entitlement and never said so out loud")
	}
}

// MEMORY.md §8: "an entitlement is declared, never inferred."
//
// The installer knows more than it is allowed to act on. A user who picks the
// Codex row — labelled "ChatGPT Login" — for the orchestrator's
// big model almost certainly holds a ChatGPT plan, and promoting that into an
// entitlement would be one line. It is forbidden, and the cost of being wrong
// is asymmetric: an entitlement overrides capability comparison, so a guessed
// one sends real work to a runtime the user is not paying for.
//
// The auth answer may be QUOTED as context, and this asserts both halves: the
// context line is printed, and the answer is still no.
func TestASubscriptionAuthChoiceIsQuotedAndNeverPromoted(t *testing.T) {
	answers := baseAnswers()
	// Codex's ChatGPT OAuth for the big model — an AuthSubscription row.
	answers["models.big.vendor"] = "openai"
	answers["models.big.auth"] = "openai-codex"
	answers["models.big.model"] = ""
	answers["models.big.chatgpt.how"] = "cli"
	delete(answers, "models.big.reuse")

	opts, script, _ := newOpts(t, answers, withCodexLogin(t))
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if got := res.Config.Routing.Entitlements; len(got) != 0 {
		t.Fatalf("choosing a subscription auth row recorded %v; §8 forbids inferring an "+
			"entitlement, and a wrong one routes real work", got)
	}
	out := script.Output()
	if !strings.Contains(out, "ChatGPT Login") {
		t.Errorf("the installer did not quote the subscription the user just chose:\n%s", out)
	}
	if !strings.Contains(out, "nothing below has been answered for you") {
		t.Error("the installer quoted the auth choice without saying it answered nothing")
	}
}

// The section has to say what the answer is FOR, in the same voice as
// claudePreamble, because "do you have a Claude subscription?" with no context
// reads as a licence check. It is not one — nothing here gives Relay the
// subscription, and a Claude plan still only works inside Claude Code.
func TestTheEntitlementQuestionSaysWhatItIsNot(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	out := script.Output()
	for _, want := range []string{
		"does not give Relay your subscription",
		"only works inside Claude Code",
		"Relay will not guess this",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the entitlement step never says %q:\n%s", want, out)
		}
	}
}

// A re-run writes the new answer and nothing else. A plan bought or lost
// changes nothing on the machine, so this command is the only way the fact ever
// gets corrected.
func TestRunEntitlementsSavesOnItsOwn(t *testing.T) {
	answers := map[string]string{
		"entitlements.claude":  "no",
		"entitlements.chatgpt": "yes",
	}
	opts, script, fs := newOpts(t, answers, nil)
	out, err := RunEntitlements(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if len(out.Entitlements) != 1 || out.Entitlements[0] != "chatgpt-subscription" {
		t.Fatalf("recorded %v", out.Entitlements)
	}

	var back config.Config
	if _, err := tomlDecode(fs.Files[opts.ConfigPath], &back); err != nil {
		t.Fatalf("the written config does not parse: %v", err)
	}
	if len(back.Routing.Entitlements) != 1 || back.Routing.Entitlements[0] != "chatgpt-subscription" {
		t.Fatalf("[routing] in the file is %v", back.Routing.Entitlements)
	}
	// The step re-runs on its own and must not have quietly undone the rest of
	// the config on the way past.
	if back.Listen != config.DefaultListen {
		t.Errorf("listen = %q after an entitlements re-run", back.Listen)
	}
}

// Every id this step can record has to be one the daemon will load. The two
// lists live in different packages by necessity — config cannot import routing
// without a cycle — so this is the seam where a typo would survive all the way
// to a config file the daemon then refuses.
func TestEveryRecordableEntitlementIsOneConfigAccepts(t *testing.T) {
	var ids []string
	for _, q := range entitlementQuestions() {
		ids = append(ids, q.entitlement)
	}
	for _, c := range codingPlanChoices() {
		ids = append(ids, c.ID)
	}
	for _, id := range ids {
		cfg := config.Default()
		cfg.Routing.Entitlements = []string{id}
		if err := cfg.Validate(); err != nil {
			t.Errorf("the installer can record %q and the daemon refuses it: %v", id, err)
		}
	}
}
