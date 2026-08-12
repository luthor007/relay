package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/acp"
	"github.com/luthor007/relay/relayd/internal/adapter/claudecode"
	"github.com/luthor007/relay/relayd/internal/adapter/codex"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/registry"
)

// defaultBinary is what each runtime is called on PATH.
//
// Five runtimes, three protocols (ADAPTERS.md §1): Claude Code speaks
// stream-json, Codex speaks app-server JSON-RPC over NDJSON, and OpenClaw,
// Hermes and OpenCode all speak ACP — so three adapter packages cover five
// binaries.
func defaultBinary(rt adapter.Runtime) string {
	switch rt {
	case adapter.ClaudeCode:
		return claudecode.DefaultBinary
	case adapter.Codex:
		return "codex"
	default:
		if rc, ok := acp.ConfigFor(rt); ok {
			return rc.Binary
		}
	}
	return ""
}

// startAdapters brings up an adapter for every runtime the config enables, and
// reports what did not come up rather than failing the daemon.
//
// A machine with two of the five installed is the normal case, not the error
// case, and ORCHESTRATOR.md §1 is explicit that the install flow has to survive
// runtimes that are absent, present-but-unauthenticated, or installed and never
// used. So a runtime that will not start is a warning with a reason attached and
// a row on the health page, never a daemon that refuses to boot.
func startAdapters(ctx context.Context, reg *registry.Registry, cfg config.Config, log *slog.Logger) []string {
	var started []string

	for _, rt := range adapter.Runtimes() {
		rc, explicit := cfg.Runtimes[string(rt)]
		if explicit && !rc.Enabled {
			continue
		}

		bin := rc.Command
		if bin == "" {
			bin = defaultBinary(rt)
		}
		if !explicit {
			// No config entry: enable it if the binary is on PATH. The installer
			// (ORCHESTRATOR.md §2) writes a real config with detection results;
			// this keeps a freshly-unpacked binary useful before it has run.
			if _, err := exec.LookPath(bin); err != nil {
				log.Debug("relayd: runtime not found on PATH", "runtime", rt, "binary", bin)
				continue
			}
		}

		a, err := dialAdapter(ctx, rt, bin, rc, log)
		if err != nil {
			// Visible, with the reason. "My glasses stopped talking" is almost
			// always this, and DASHBOARD.md §3.5 wants a page that already says
			// which runtime and why.
			log.Warn("relayd: could not start a runtime adapter",
				"runtime", rt, "binary", bin, "error", err)
			continue
		}
		reg.AddAdapter(a)
		started = append(started, string(rt))
	}
	return started
}

func dialAdapter(
	ctx context.Context,
	rt adapter.Runtime,
	binary string,
	rc config.RuntimeConfig,
	log *slog.Logger,
) (adapter.Adapter, error) {
	log = log.With("runtime", string(rt))

	switch rt {
	case adapter.ClaudeCode:
		// Nothing is launched here: Claude Code runs one process per session and
		// the adapter starts them lazily.
		return claudecode.New(claudecode.Options{Binary: binary, Log: log}), nil

	case adapter.Codex:
		// One app-server connection hosts many threads, so this dials now.
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return codex.Dial(dctx, codex.Options{
			Binary:        binary,
			Log:           log,
			ClientName:    "relay",
			ClientVersion: version,
		})

	case adapter.OpenClaw, adapter.Hermes, adapter.OpenCode:
		var env []string
		if rc.StateDir != "" {
			// Never hardcode ~/.openclaw: OPENCLAW_STATE_DIR, --profile and --dev
			// all relocate it, and a reader that assumes the default silently
			// finds nothing and reports an empty history as success.
			env = append(env, "OPENCLAW_STATE_DIR="+rc.StateDir)
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return acp.Dial(dctx, acp.Options{
			Runtime: rt,
			Binary:  binary,
			Env:     env,
			Log:     log,
		})
	}
	return nil, fmt.Errorf("relayd: no adapter for runtime %q", rt)
}
