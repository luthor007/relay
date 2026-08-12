//go:build linux

package apps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// Linux isolation: namespaces the kernel will give an unprivileged process, and
// rlimits applied through `prlimit64` before the child is told to do anything.
//
// Nothing here needs cgo, a container runtime, root, or a cgroup delegation —
// the four things a self-hoster's box may not have. It needs unprivileged user
// namespaces, which most distributions have enabled for years, and it *measures*
// whether it got them instead of assuming.
//
// # The network namespace is the egress boundary
//
// `CLONE_NEWNET` gives the child an empty network namespace: a `lo` that is
// down, no route, no interface. A connect() from inside returns `ENETUNREACH`
// before it reaches the name resolution, let alone the wire. That is what makes
// APP-PLATFORM.md §3's "egress is default-deny" a fact about the kernel rather
// than a policy the app is asked to respect — and it is why the capability
// channel travels on a pipe (fds 3 and 4) rather than on a socket: a socket
// would need a network the app must not have.
//
// # The uid mapping
//
// The child is mapped to 65534 (nobody) inside the namespace. It is a *mapped*
// nobody — the kernel still accounts it to the uid relayd runs as, which is why
// [Limits] does not use `RLIMIT_NPROC` — but it means the app process has no
// privilege inside its own namespace either, so a file left group- or
// world-writable by accident is not a path back.

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// isolation is the measured set of namespaces this kernel will hand out.
type isolation struct {
	name  string
	flags uintptr
	net   bool
	pid   bool
	user  bool
}

func isolationNone() isolation { return isolation{name: "process-only"} }

// levels, strongest first. Each is tried by [probeIsolation] with a real
// process; the first that starts is the one used for every app afterwards.
var levels = []isolation{
	{
		name: "linux-namespaces",
		flags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		net: true, pid: true, user: true,
	},
	{
		// Some kernels refuse CLONE_NEWPID to an unprivileged process while
		// still allowing the rest. Losing the PID namespace costs process
		// invisibility; it does not cost the egress boundary, which is the one
		// worth degrading last.
		name:  "linux-namespaces-nopid",
		flags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWUTS,
		net:   true, user: true,
	},
	{
		// User namespaces available, network not (seccomp filters on some
		// hosted platforms block CLONE_NEWNET specifically). This is the level
		// at which an app holding memory.read stops being runnable — see
		// [ErrCannotContain].
		name:  "linux-userns",
		flags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
		user:  true,
	},
	isolationNone(),
}

// probeIsolation starts the runtime binary under each level and keeps the first
// that works. It costs one process start at daemon boot and buys an
// [Enforcement] that is measured rather than declared.
func probeIsolation(ctx context.Context, exe string, args []string) isolation {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, lvl := range levels {
		if lvl.flags == 0 {
			return lvl
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(c, exe, args...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		lvl.apply(cmd)
		err := cmd.Run()
		cancel()
		if err == nil {
			return lvl
		}
	}
	return isolationNone()
}

func (i isolation) apply(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{
		Setpgid: true,
		// If relayd dies, the app dies. An orphaned app process holding the
		// user's box after the daemon that was supervising it is gone is exactly
		// the failure the wall-clock cap exists to prevent, and a supervisor
		// cannot enforce a clock it is no longer alive to read.
		Pdeathsig: syscall.SIGKILL,
	}
	if i.flags != 0 {
		attr.Cloneflags = i.flags
		attr.Unshareflags = syscall.CLONE_NEWNS
	}
	if i.user {
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 65534, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 65534, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr = attr
}

func (i isolation) guarantees() Enforcement {
	var e Enforcement

	if i.net {
		e.Network = Enforced("linux network namespace",
			"the app process has no interface and no route: a connect() fails with ENETUNREACH "+
				"before any name is resolved. ctx.fetch is run by relayd against the manifest allowlist")
	} else {
		e.Network = Declared(
			"this kernel would not give the app its own network namespace, so the app process can " +
				"open sockets relayd does not see. The manifest allowlist is enforced on ctx.fetch only")
	}

	switch {
	case i.pid && i.user:
		e.Processes = Enforced("linux pid/ipc/uts namespaces and a process group",
			"the app cannot see, signal or inherit anything of relayd's, and the whole group is killed together")
	case i.user:
		e.Processes = Partial("linux mount/uts namespaces and a process group",
			"process creation is refused by the runtime and the group is killed together, but the app "+
				"can see the host's process list")
	default:
		e.Processes = Partial("process group",
			"the group is killed together; nothing hides the host's processes from the app")
	}

	if i.user {
		e.User = Enforced("linux user namespace",
			"the app runs as nobody inside its own namespace, with no privilege there")
	} else {
		e.User = Declared("the app runs as the same user as relayd")
	}
	return e
}

// ------------------------------------------------------------- rlimits --

type rlimit64 struct{ Cur, Max uint64 }

// resource numbers, from <asm-generic/resource.h>. syscall exposes RLIMIT_AS and
// RLIMIT_NOFILE but not RLIMIT_CPU or RLIMIT_FSIZE on every Go release, so they
// are named here rather than depending on which.
const (
	rlimitCPU    = 0
	rlimitFsize  = 1
	rlimitCore   = 4
	rlimitAS     = 9
	rlimitNofile = 7
)

func prlimit(pid int, resource uintptr, lim *rlimit64) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), resource,
		uintptr(unsafe.Pointer(lim)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// limitExpectation is what applying the caps *should* achieve on this platform.
// [Runtime] reports it before any app has run; [applyLimits] returns the same
// thing with any control that actually failed downgraded, and that per-run
// version is what lands on the [Invocation].
func limitExpectation(l Limits) *limitReport {
	l = l.withDefaults()
	cpu := cpuSeconds(l)
	as, raised := l.AddressSpaceRaised()
	note := fmt.Sprintf(
		"the JS heap is capped at %s by V8, which is the cap that binds. Buffers, native allocations "+
			"and the code range are outside it", megabytes(l.Memory))
	switch {
	case as == 0:
		note += ", and RLIMIT_AS is not applied: the runtime needs about 22 GiB of virtual " +
			"reservation to start at all, so an address-space limit is either unreachable or fatal"
	case raised:
		note += fmt.Sprintf("; RLIMIT_AS is a %s backstop against a runaway mapping — the requested %s "+
			"was raised to the floor the runtime needs to start", megabytes(as), megabytes(l.AddressSpace))
	default:
		note += fmt.Sprintf("; RLIMIT_AS is a %s backstop against a runaway mapping, not a memory cap",
			megabytes(as))
	}
	return &limitReport{
		CPU: Enforced("prlimit RLIMIT_CPU",
			fmt.Sprintf("%ds of processor time, then SIGXCPU and SIGKILL. Wall-clock time spent waiting "+
				"is not processor time, so the wall-clock cap is the one that catches a sleeping app", cpu)),
		Memory: Partial(memoryMechanism(as), note),
		FileSize: Enforced("prlimit RLIMIT_FSIZE",
			fmt.Sprintf("a single file over %s is refused with SIGXFSZ", megabytes(l.MaxFileSize))),
	}
}

// memoryMechanism names what is actually holding the memory cap, which is not
// the same list on a box that opted into RLIMIT_AS as on one that did not.
func memoryMechanism(addressSpace int64) string {
	if addressSpace > 0 {
		return "node --max-old-space-size and prlimit RLIMIT_AS"
	}
	return "node --max-old-space-size"
}

func cpuSeconds(l Limits) uint64 {
	cpu := uint64(l.CPUTime / time.Second)
	if cpu < 1 {
		cpu = 1
	}
	return cpu
}

// applyLimits sets the caps on an already-started child.
//
// It runs between fork/exec and the start frame the runner is blocking on, so
// nothing of the app's has run yet. Go gives no hook between fork and exec — it
// cannot, safely — so this ordering, and the runner's handshake that depends on
// it, is how the caps come to be in force before app code loads.
func applyLimits(pid int, l Limits) *limitReport {
	l = l.withDefaults()
	rep := limitExpectation(l)

	cpu := cpuSeconds(l)
	// Soft one second under hard: the soft limit raises SIGXCPU, whose default
	// action terminates, and the hard limit is the kernel's SIGKILL backstop for
	// a process that caught SIGXCPU and carried on.
	if err := prlimit(pid, rlimitCPU, &rlimit64{Cur: cpu, Max: cpu + 1}); err != nil {
		rep.CPU = Declared(fmt.Sprintf("RLIMIT_CPU could not be set (%v)", err))
	}

	if as, _ := l.AddressSpaceRaised(); as > 0 {
		if err := prlimit(pid, rlimitAS, &rlimit64{Cur: uint64(as), Max: uint64(as)}); err != nil {
			rep.Memory = Partial("node --max-old-space-size",
				fmt.Sprintf("RLIMIT_AS could not be set (%v), so only the JS heap is capped, at %s. "+
					"Buffers and native allocations are not", err, megabytes(l.Memory)))
		}
	}

	if err := prlimit(pid, rlimitFsize, &rlimit64{Cur: uint64(l.MaxFileSize), Max: uint64(l.MaxFileSize)}); err != nil {
		rep.FileSize = Declared(fmt.Sprintf("RLIMIT_FSIZE could not be set (%v)", err))
	}

	_ = prlimit(pid, rlimitNofile, &rlimit64{Cur: uint64(l.MaxOpenFiles), Max: uint64(l.MaxOpenFiles)})
	// No core dumps: a core of an app process is a file containing whatever of
	// the user's memory that app had read.
	_ = prlimit(pid, rlimitCore, &rlimit64{Cur: 0, Max: 0})
	return rep
}

func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil {
		// A process that has already reaped, or one that never got its own
		// group, still deserves the direct signal.
		return syscall.Kill(pid, sig)
	}
	return nil
}
