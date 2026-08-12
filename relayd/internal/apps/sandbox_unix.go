//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package apps

import (
	"context"
	"os/exec"
	"syscall"
)

// The non-Linux Unix sandbox: a process group, and honesty about the rest.
//
// macOS is the platform SYSTEM.md §8's pricing story cross-compiles to, and it
// is where a developer runs relayd on their own laptop. It has no namespaces. It
// has `sandbox_init`, which is deprecated, undocumented and reachable only
// through cgo — and cgo is the one thing this daemon may not have, because the
// single static binary that cross-compiles to four targets is the product.
//
// So what this file offers is a process group that can be killed as a unit, and
// an [Enforcement] that says plainly what is missing. Everything real on this
// platform comes from the layer above: the runtime's permission model confines
// the filesystem and refuses process creation, and the supervisor holds the wall
// clock. Network isolation is **declared**, which is why [Runtime] refuses by
// default to run an app holding a scope that reads the user's life here.
//
// That refusal is the point. Quietly running the same app with a weaker boundary
// on macOS, because macOS is where the developer is, is how a sandbox becomes a
// thing that is true on the machines nobody uses.

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

type isolation struct{ name string }

func isolationNone() isolation { return isolation{name: "process-only"} }

func probeIsolation(context.Context, string, []string) isolation {
	return isolation{name: "process-only"}
}

func (i isolation) apply(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (i isolation) guarantees() Enforcement {
	return Enforcement{
		Network: Declared(
			"this platform has no network namespace reachable without cgo, so the app process can open " +
				"sockets relayd does not see. The manifest allowlist is enforced on ctx.fetch only"),
		Processes: Partial("process group",
			"the app and anything it started are killed together; nothing hides the host's processes from it"),
		User: Declared("the app runs as the same user as relayd"),
	}
}

// limitExpectation, and applyLimits, report what this platform's Go syscall
// surface offers on *another* process, which is nothing: there is no prlimit(2)
// here, and setrlimit applies only to the caller. Reporting that is the whole
// job — a memory cap described as enforced on a platform that cannot set one is
// the exact failure this package refuses to commit.
func limitExpectation(l Limits) *limitReport {
	l = l.withDefaults()
	return &limitReport{
		CPU: Declared("no per-child rlimit on this platform; the wall-clock cap is what stops a busy app"),
		Memory: Partial("node --max-old-space-size",
			"the JS heap is capped at "+megabytes(l.Memory)+
				". Buffers and native allocations are not, and there is no address-space cap here"),
		FileSize: Declared("no per-child RLIMIT_FSIZE on this platform"),
	}
}

func applyLimits(_ int, l Limits) *limitReport { return limitExpectation(l) }

func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}
