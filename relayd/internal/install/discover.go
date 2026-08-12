package install

import (
	"context"
	"errors"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/vault"
)

// MEMORY.md §6's second arrival path, which is the installer's to run.
//
// §6 ranks the three ways a key arrives and says of this one: runtimes already
// store provider credentials in known places, "enumerable at install with the
// user watching". The watching is the point. This is the one moment when the
// person whose keys these are is in front of the terminal, and it is therefore
// both the best time to ask and the worst possible time to take something
// without asking.
//
// So this step proposes and never stores. vault.Discover reads the configs and
// returns candidates; every one becomes a question in the queue, and the
// console or the app is where somebody answers it. An unattended install that
// silently swallowed a colleague's key off a shared box is the failure this
// shape exists to make impossible — and internal/install's own test asserts
// Propose was called and Put was not.

// DiscoveryOutcome is what the config scan found, for the summary and for tests.
type DiscoveryOutcome struct {
	// Proposed is the number of new questions this run added to the queue.
	Proposed int
	// Known is candidates already in the vault, and Decided is ones the user has
	// already answered. Both are normal on a second install and neither is a
	// finding worth showing.
	Known, Decided int
	// Files is what was read.
	Files []string
	// Unreadable is files that exist and would not open or would not parse.
	// MEMORY.md §7's rule: "not there" and "could not read it" lead to opposite
	// decisions, so they are never the same empty list. A key could be in any of
	// these.
	Unreadable []string
	// Skipped says why nothing ran, when nothing ran.
	Skipped string
}

// discoverConfigKeys scans the runtimes' own config files and proposes what it
// finds.
func discoverConfigKeys(ctx context.Context, opts Options) DiscoveryOutcome {
	var out DiscoveryOutcome
	p := opts.Prompt

	q := proposalQueue(opts.Vault)
	if q == nil {
		// Without a queue there is nowhere to put a question, and the only other
		// place to put a found key would be the vault itself — which is exactly
		// what this step must not do. Saying so beats scanning and discarding.
		out.Skipped = "no credential vault in this run, so nothing can be proposed"
		return out
	}

	found, err := vault.Discover(ctx, vault.DiscoverOptions{
		Home:      opts.Env.Home,
		XDGData:   opts.Env.Getenv("XDG_DATA_HOME"),
		XDGConfig: opts.Env.Getenv("XDG_CONFIG_HOME"),
		FS:        opts.Env.FS,
		Now:       opts.Now,
	})
	if err != nil {
		out.Skipped = err.Error()
		return out
	}
	out.Files = found.Read
	out.Unreadable = found.Unreadable

	p.Section("Keys already on this machine",
		"Your agent runtimes keep provider credentials in their own config files. Relay "+
			"can look, and anything it finds becomes a question rather than a saved key — "+
			"nothing is stored until you answer it in the console.")

	for _, c := range found.Candidates {
		prop, err := q.Propose(ctx, c)
		switch {
		case err == nil && prop.Open():
			out.Proposed++
			// Last four, never more, even here — the installer's output is the
			// most-screenshotted surface this product has.
			p.Say("  Found a %s key in %s (…%s). It is waiting in the console as a question.",
				c.Service, c.Source.Path, prop.LastFour)
		case err == nil, errors.Is(err, vault.ErrDecided):
			out.Decided++
		case errors.Is(err, vault.ErrKnown):
			out.Known++
		default:
			// A candidate that would not seal is not a reason to abandon the
			// install, and it is not something to be quiet about either.
			p.Say("  Could not propose the %s key in %s: %v", c.Service, c.Source.Path, err)
		}
	}

	switch {
	case out.Proposed == 0 && len(found.Read) == 0:
		p.Say("  No runtime config files on this machine held anything credential-shaped.")
	case out.Proposed == 0:
		p.Say("  Nothing new — every key in those files is already held or already answered.")
	}
	for _, u := range out.Unreadable {
		// Different from "not there", and deliberately so. A file that exists
		// and would not parse may hold a key we never saw.
		p.Say("  Could not read %s, so anything in it was not looked at.", u)
	}
	return out
}

// proposalQueue is the queue half of the vault, when the installer was given a
// vault that has one.
//
// [Options.Vault] is deliberately narrow — Put and nothing else — because the
// only thing the credential step needs is to store a key the user typed. The
// discovery step needs the opposite capability and must not gain Put's, so it
// asks for the queue separately and does nothing when it is absent.
func proposalQueue(v Vault) vault.Proposals {
	if v == nil {
		return nil
	}
	q, ok := v.(interface{ Proposals() vault.Proposals })
	if !ok {
		return nil
	}
	return q.Proposals()
}

// line summarises the outcome for the installer's closing report.
func (d DiscoveryOutcome) line() string {
	switch {
	case d.Skipped != "":
		return "Config key scan: " + d.Skipped
	case d.Proposed == 1:
		return "1 key found in a runtime's own config is waiting in the console"
	case d.Proposed > 1:
		return fmt.Sprintf("%d keys found in runtime configs are waiting in the console", d.Proposed)
	}
	return ""
}
