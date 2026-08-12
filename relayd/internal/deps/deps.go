// Package deps pins the modules the rest of this milestone needs but that no
// Foundation code imports yet.
//
// Only the Foundation agent may touch go.mod and go.sum — concurrent writes to
// the module graph corrupt it, and five agents are about to work in parallel on
// top of this. So every dependency the milestone will need is added up front,
// and the ones nothing imports yet are held here so `go mod tidy` cannot drop
// them and go.sum stays complete.
//
// Delete a line from this file when a real package starts importing that
// module. Do not add a line without also adding the module to go.mod, which
// means: do not add a line.
package deps

import (
	// The phone talks to relayd over one authenticated WebSocket carrying JSON
	// envelopes, both directions (SYSTEM.md §6.1). internal/api will import it.
	_ "github.com/coder/websocket"

	// OpenClaw and OpenCode both keep YAML config, and the installer has to
	// read theirs to find where their state actually lives. internal/detect
	// and internal/backfill will import it.
	_ "gopkg.in/yaml.v3"

	// The installer's prompts: a password read that does not echo, and a
	// terminal width for the two-level vendor menu. internal/install will
	// import it.
	_ "golang.org/x/term"
)
