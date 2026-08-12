package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/luthor007/relay/relayd/internal/backfill"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// backfillCmd indexes every session that already exists on this machine.
//
// MEMORY.md §1: ~3.6 GB of history exists before we ship anything, so the first
// run of the installer is not a cold start, it is an archaeology job — and that
// is the best thing about this product's first five minutes, because it can know
// the user's stack before they have said a word to it.
//
// The five readers were written and tested and had no caller, so nothing had
// ever been indexed and the whole memory tier sat behind a door with no handle.
// This is the handle.
func backfillCmd(ctx context.Context, g globals, rest []string, out io.Writer) error {
	// The global flagset has already run, so these are subcommand words rather
	// than flags. Keeping --force global matches `relay reindex`, which means
	// the same word for the same idea across both.
	var dryRun bool
	for _, a := range rest {
		switch a {
		case "--dry-run", "-dry-run":
			dryRun = true
		default:
			return fmt.Errorf("relay backfill: unknown argument %q", a)
		}
	}
	force := g.force

	cfg, err := loadConfigFor(g.configPath, "")
	if err != nil {
		return err
	}
	dbPath, err := cfg.DBPath()
	if err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	env := backfill.OSEnv()
	readers := backfill.Readers(env)
	det, err := index.NewDetector()
	if err != nil {
		return err
	}
	indexer := index.New(db, det)

	// MEMORY.md §6's third arrival path needs somewhere to put the question.
	// Detection has always run here — it has to, because an embedded key cannot
	// be unembedded — but the findings were counted and dropped, so the console
	// advertised a proposal queue that was empty by construction. A vault that
	// will not open is reported and indexing continues: the index is the job,
	// and losing it because a keychain was locked would be the wrong trade.
	var queue proposer
	if !dryRun {
		queue = openProposals(ctx, cfg, out)
		if queue.v != nil {
			defer queue.v.Close()
		}
	}

	fmt.Fprintf(out, "backfill  %d readers, database %s\n\n", len(readers), dbPath)

	var scanned, indexed, skipped, failed int
	started := time.Now()

	for _, r := range readers {
		rt := r.Runtime()
		res, err := r.Scan(ctx)
		if err != nil {
			// A reader that cannot read its own store is worth one line and no
			// more. MEMORY.md §1 found two of five runtimes with no data at all
			// on a real machine, so "nothing here" is the normal case rather than
			// an error, and one broken store must not stop the other four.
			fmt.Fprintf(out, "  %-10s unreadable: %v\n", rt, err)
			failed++
			continue
		}

		fmt.Fprintf(out, "  %-10s %s, %d sessions\n", rt, res.Status, len(res.Refs))
		// "Nothing found" always comes with "and here is where we looked" — a
		// reader that assumes a path and silently finds nothing reports an empty
		// history as success, which is the OpenClaw trap in MEMORY.md §4.
		if len(res.Refs) == 0 {
			for _, root := range res.Roots {
				fmt.Fprintf(out, "             looked in %s\n", root)
			}
		}
		for _, note := range res.Notes {
			fmt.Fprintf(out, "             %s\n", note)
		}

		for _, ref := range res.Refs {
			scanned++

			if !force {
				// Incremental and resumable, keyed on (runtime, session_id,
				// mtime). 3.6 GB through a small model is an hour or two and has
				// to survive being interrupted.
				need, err := indexer.NeedsIndexing(ctx, rt, ref.SessionID, ref.MTime, ref.Size)
				if err != nil {
					return err
				}
				if !need {
					skipped++
					continue
				}
			}

			if dryRun {
				indexed++
				continue
			}

			sess, err := r.Read(ctx, ref)
			if err != nil {
				fmt.Fprintf(out, "             %s: %v\n", ref.SessionID, err)
				failed++
				continue
			}

			// Index runs the secret detector over everything it is about to
			// persist before it persists any of it. An embedded key cannot be
			// unembedded, so the ordering is the code path, not a convention.
			res, err := indexer.Index(ctx, sess)
			if err != nil {
				fmt.Fprintf(out, "             %s: %v\n", ref.SessionID, err)
				failed++
				continue
			}
			indexed++

			for _, s := range res.MarkerSentences() {
				fmt.Fprintf(out, "             %s\n", s)
			}

			// A tier-1 finding becomes a question, never a stored key. Tier 2
			// never reaches here at all: VaultCandidates filters it out because
			// §12.2 measured one in four of those as a checksum.
			queue.propose(ctx, out, res.VaultCandidates(), sess)
		}
	}

	fmt.Fprintf(out, "\n%d scanned, %d indexed, %d already current, %d failed, in %s\n",
		scanned, indexed, skipped, failed, time.Since(started).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(out, "\ndry run — nothing was written")
		return nil
	}

	// The vault proposals are the point of the detector finding anything, and
	// MEMORY.md §6 is emphatic that nothing is captured silently: a match is a
	// proposal the user accepts or dismisses, never a stored credential. This
	// paragraph used to promise a queue nothing filled, so it now reports the
	// count it actually made rather than describing a flow.
	queue.summarise(out)
	return nil
}

// proposer is `relay backfill`'s half of MEMORY.md §6, or a no-op.
//
// The zero value is the honest degraded state: a machine whose vault will not
// open still gets its history indexed, and every method below does nothing
// rather than the command refusing to run.
type proposer struct {
	v vault.Vault
	q vault.Proposals

	// Counted separately because they mean different things to the person
	// reading the last paragraph. New is a question waiting for them; known and
	// decided are the normal outcome of a re-run and not worth a line each.
	asked, known, decided, failed int
}

// openProposals opens the vault beside the index, or says why it could not.
func openProposals(ctx context.Context, cfg config.Config, out io.Writer) proposer {
	path, err := cfg.VaultPath()
	if err == nil {
		var v vault.Vault
		if v, err = vaultOpen(ctx, vault.Options{DBPath: path}); err == nil {
			return proposer{v: v, q: v.Proposals()}
		}
	}
	// Worth a line and no more. Indexing is what this command is for, and a
	// silent skip would leave the user believing detections became proposals.
	fmt.Fprintf(out, "note      the credential vault would not open (%v);\n", err)
	fmt.Fprintln(out, "          sessions are still indexed, but nothing can be proposed this run")
	return proposer{}
}

// propose turns one session's tier-1 findings into questions.
//
// Nothing here prints or logs a secret. LastFour is the display form and the
// only one — the plaintext crosses this function on its way into AES-GCM and is
// referred to afterwards by its last four characters.
func (p *proposer) propose(ctx context.Context, out io.Writer, found []index.Finding, sess index.Session) {
	if p.q == nil {
		return
	}
	for _, f := range found {
		c, ok := vault.FromFinding(f, vault.Provenance{
			Kind:    vault.SourceTranscript,
			Runtime: string(sess.Runtime),
			Session: sess.SessionID,
			Path:    sess.Path,
			At:      sess.StartedAt,
			// SharedSession stays false, and false here means "not observed"
			// rather than "checked and no". Nothing in the readers, the index
			// or the store records whether a session had another participant,
			// and inventing it from a heuristic would be an adapter emitting an
			// event it cannot see. MEMORY.md §6's "a key in your transcript may
			// not be yours" therefore cannot fire yet; HANDOFF.md records it.
		})
		if !ok {
			continue
		}
		prop, err := p.q.Propose(ctx, c)
		switch {
		case err == nil && prop.Open():
			p.asked++
			fmt.Fprintf(out, "             proposal: %s (…%s) — accept or dismiss it in the console\n",
				prop.Line(), prop.LastFour)
		case err == nil:
			p.decided++
		case errors.Is(err, vault.ErrKnown):
			// Already in the vault. Asking again is how a queue becomes noise
			// somebody learns to dismiss unread.
			p.known++
		case errors.Is(err, vault.ErrDecided):
			p.decided++
		case errors.Is(err, vault.ErrNotProposable):
			// Cannot happen from VaultCandidates, which is tier 1 only. Counted
			// rather than ignored so a future caller that widens the filter
			// shows up as a number instead of as silence.
			p.decided++
		default:
			p.failed++
			fmt.Fprintf(out, "             could not propose a %s credential: %v\n", c.Service, err)
		}
	}
}

// summarise is the closing paragraph, and it reports rather than promises.
func (p *proposer) summarise(out io.Writer) {
	switch {
	case p.q == nil:
		return
	case p.asked == 1:
		fmt.Fprintln(out, "\n1 credential proposal is waiting in the console. It is a question, not a")
		fmt.Fprintln(out, "saved key — nothing was stored, and dismissing it is a complete answer.")
	case p.asked > 1:
		fmt.Fprintf(out, "\n%d credential proposals are waiting in the console. They are questions, not\n", p.asked)
		fmt.Fprintln(out, "saved keys — nothing was stored, and dismissing them is a complete answer.")
	case p.known+p.decided > 0:
		fmt.Fprintf(out, "\nNo new credential proposals (%d already held or already answered).\n",
			p.known+p.decided)
	}
	if p.failed > 0 {
		fmt.Fprintf(out, "%d detection(s) could not be turned into a proposal; they stay redacted in the index.\n",
			p.failed)
	}
}

// loadConfigFor resolves the config the same way relayd does, so `relay
// backfill` writes to the database the daemon will read.
func loadConfigFor(configPath, dataDir string) (config.Config, error) {
	if configPath == "" {
		p, err := config.Path()
		if err != nil {
			return config.Config{}, err
		}
		configPath = p
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, err
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	return cfg, nil
}
