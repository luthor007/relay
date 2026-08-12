//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package apps

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// Everything that is neither Linux nor a BSD.
//
// SYSTEM.md §8's four targets are darwin and linux on arm64 and amd64, so this
// file exists to keep the package compiling rather than to offer containment.
// It offers none, and it says so in every field of [Enforcement] — which means
// [Runtime]'s default policy will refuse to run any app holding a scope that
// reads the user's life here. A platform where nothing can be contained is a
// platform where untrusted code should not be run, and the honest way to express
// that is a refusal rather than a smaller promise.

const (
	sigTerm = syscall.Signal(syscall.SIGTERM)
	sigKill = syscall.Signal(syscall.SIGKILL)
)

type isolation struct{ name string }

func isolationNone() isolation { return isolation{name: "uncontained"} }

func probeIsolation(context.Context, string, []string) isolation { return isolationNone() }

func (i isolation) apply(*exec.Cmd) {}

func (i isolation) guarantees() Enforcement {
	none := Declared("this platform has no containment relayd can use without cgo")
	return Enforcement{Network: none, Processes: none, User: none}
}

func limitExpectation(l Limits) *limitReport {
	l = l.withDefaults()
	return &limitReport{
		CPU: Declared("no per-child resource limits on this platform"),
		Memory: Partial("node --max-old-space-size",
			"the JS heap is capped at "+megabytes(l.Memory)+"; nothing else is"),
		FileSize: Declared("no per-child file-size limit on this platform"),
	}
}

func applyLimits(_ int, l Limits) *limitReport { return limitExpectation(l) }

// signalGroup falls back to the process itself. There is no process group to
// signal here, so a child the app started outlives the kill — which is one more
// reason this platform refuses to run apps that hold real scopes.
func signalGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}
