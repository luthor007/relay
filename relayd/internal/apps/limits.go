package apps

import (
	"fmt"
	"time"
)

// Resource caps — APP-PLATFORM.md §5's last bullet: "CPU, memory and wall-clock
// per invocation; an app that hangs is killed, not left holding the box."
//
// Three of those four numbers are enforced by something outside the app, and one
// of them is enforced in two halves by two different things. That is not a
// compromise to be embarrassed about, it is what the platform offers, and the
// only way to get it wrong is to report it as one number.
//
// # The memory cap is V8's, and RLIMIT_AS is off by default. Measured.
//
// The obvious implementation — `RLIMIT_AS`, one number, kernel-enforced — does
// not survive contact with the runtime. Measured on Node 22.22 / Linux amd64,
// with `prlimit64` applied to the child immediately after `exec`:
//
//	RLIMIT_AS   plain JS      a .ts entry point   .ts plus one fetch/Response
//	512 MiB     dies in V8    dies                dies
//	1 GiB       starts        dies                dies
//	8 GiB       starts        dies                dies
//	12 GiB      starts        starts              dies
//	20 GiB      starts        starts              dies
//	22 GiB      starts        starts              starts
//
// Both cliffs are WebAssembly reservations, and both are on the normal path
// rather than at an edge: an app's entry point is `src/index.ts` in
// APP-PLATFORM.md §2, so Node's type stripper always loads, and `ctx.fetch`
// builds a `Response`, which pulls in undici's WASM HTTP parser. An `RLIMIT_AS`
// low enough to be a *memory cap* is one that stops the runtime from starting;
// one high enough to start is twenty gigabytes, which caps nothing a box could
// survive anyway.
//
// So `RLIMIT_AS` is **not applied by default**. Setting a 22 GiB limit and
// calling memory "kernel-enforced" would be the exact overstatement this package
// exists to refuse. What is left is real and is reported as exactly itself:
//
//   - [Limits.Memory] becomes `--max-old-space-size`. V8 enforces it, and an app
//     that allocates past it dies with a heap OOM — see
//     TestAnAppThatEatsMemoryIsKilled. This is the cap that binds. It does
//     **not** cover `ArrayBuffer`s, native allocations, or the code range.
//   - [Limits.AddressSpace] is opt-in, for a deployment that ships precompiled
//     JavaScript apps and can therefore afford a real ceiling. Zero means not
//     applied, and anything below [MinAddressSpace] is raised to it with the
//     raise reported, because a limit that prevents the runtime from starting is
//     a limit that turns every app into a crash.
//
// [Enforcement.Memory] is therefore [ControlPartial], always, on every platform,
// and its note says which half is which and which is absent.
//
// # What is deliberately not capped
//
// `RLIMIT_NPROC` is not applied, and the reason is worth writing down so nobody
// adds it as an oversight fix: the limit is counted per *real uid*, system-wide,
// not per process tree. Under a user namespace the app's uid maps back to the
// uid relayd runs as, so a low `NPROC` would count relayd's own threads and the
// user's other work — and the failure mode is relayd being unable to start a
// thread, which is worse than the fork bomb it was meant to stop. Process
// creation is refused by the runtime's permission model instead, and the PID
// namespace keeps what does exist invisible.

// Limits is one invocation's resource envelope.
type Limits struct {
	// WallClock is the ceiling on one invocation. Enforced by the supervisor:
	// SIGTERM to the process group, [Limits.Grace], then SIGKILL.
	WallClock time.Duration
	// Grace is how long a terminating app has to exit before it is killed.
	Grace time.Duration
	// CPUTime is processor seconds. Enforced by RLIMIT_CPU, at one-second
	// granularity — a value under a second is raised to one, because the kernel
	// counts in seconds and silently rounding to zero would kill every app
	// instantly.
	CPUTime time.Duration
	// Memory caps the JS heap. See the note above.
	Memory int64
	// AddressSpace caps total virtual memory through RLIMIT_AS. **Zero, the
	// default, means it is not applied at all** — see the measurements above.
	// A non-zero value below [MinAddressSpace] is raised to it, and the raise is
	// reported rather than silent.
	AddressSpace int64
	// MaxFileSize caps a single file the app writes to scratch.
	MaxFileSize int64
	// MaxOpenFiles caps descriptors.
	MaxOpenFiles int64
}

// Defaults. They are sized for "an app summarises a meeting", not for "an app
// trains a model": the box is the user's, and an app that wants more of it than
// this should be a conversation rather than a default.
const (
	DefaultWallClock = 30 * time.Second
	DefaultGrace     = 2 * time.Second
	DefaultCPUTime   = 20 * time.Second
	// DefaultMemory is the JS heap ceiling, and it is the number that actually
	// binds an app's allocation.
	DefaultMemory = 256 << 20
	// DefaultAddressSpace is zero: RLIMIT_AS is not applied unless a caller asks
	// for it. See the table above for why.
	DefaultAddressSpace = 0
	DefaultMaxFileSize  = 64 << 20
	DefaultMaxOpenFiles = 256

	// MinAddressSpace is the floor below which the runtime cannot start with a
	// TypeScript entry point that also uses `ctx.fetch`. Measured: 20 GiB dies
	// in undici's WebAssembly allocation, 22 GiB starts; 24 GiB is that with
	// margin, because a floor measured to the gigabyte on one Node build is a
	// floor that moves on the next one.
	MinAddressSpace = 24 << 30
)

// DefaultLimits is the envelope an app gets when the caller says nothing.
func DefaultLimits() Limits {
	return Limits{
		WallClock:    DefaultWallClock,
		Grace:        DefaultGrace,
		CPUTime:      DefaultCPUTime,
		Memory:       DefaultMemory,
		AddressSpace: DefaultAddressSpace,
		MaxFileSize:  DefaultMaxFileSize,
		MaxOpenFiles: DefaultMaxOpenFiles,
	}
}

// withDefaults fills the zero fields and applies the two floors.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.WallClock <= 0 {
		l.WallClock = d.WallClock
	}
	if l.Grace <= 0 {
		l.Grace = d.Grace
	}
	if l.CPUTime <= 0 {
		l.CPUTime = d.CPUTime
	}
	if l.CPUTime < time.Second {
		l.CPUTime = time.Second
	}
	if l.Memory <= 0 {
		l.Memory = d.Memory
	}
	// AddressSpace is deliberately not defaulted: zero means "do not apply
	// RLIMIT_AS", and that is the default.
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = d.MaxFileSize
	}
	if l.MaxOpenFiles <= 0 {
		l.MaxOpenFiles = d.MaxOpenFiles
	}
	return l
}

// AddressSpaceRaised reports the RLIMIT_AS that will actually be applied, and
// whether the floor had to raise the requested one.
//
// A zero first return means RLIMIT_AS is not applied at all, which is the
// default. [Runtime] puts both facts in the guarantee's note: an app configured
// for 128 MiB that is really running with twenty-four gigabytes of address space
// must not be described as capped at 128 MiB.
func (l Limits) AddressSpaceRaised() (int64, bool) {
	as := l.AddressSpace
	if as <= 0 {
		return 0, false
	}
	if as < MinAddressSpace {
		return MinAddressSpace, true
	}
	return as, false
}

// HeapMB is the `--max-old-space-size` value for these limits.
func (l Limits) HeapMB() int {
	mb := int(l.withDefaults().Memory >> 20)
	if mb < 16 {
		mb = 16
	}
	return mb
}

func megabytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%d MiB", n>>20)
}
