// Package apps is the app runtime on the box — APP-PLATFORM.md §8 step 2.
//
// §1 makes the call this package exists to pay for: **apps run on the user's
// own machine, not on the developer's server.** The author never receives the
// transcript, which for a device that records someone's working day is the
// difference between a product people will wear and one they will not. The bill
// for that is stated in the same section and lands here: *a malicious package
// runs on the user's machine.* §5 is about containing it, and containing it is
// this package's whole job.
//
// # What is enforced, and what is only declared
//
// The single most important thing about this package is that it never claims a
// boundary it did not get. [Enforcement] is a value, not a sentence in a README:
// every control carries [Control] — enforced, degraded, or declared — the name
// of the mechanism that enforces it, and a note saying exactly how far it goes.
// [Runtime.Enforcement] is measured once at startup by actually running a probe
// process, because "the kernel allows user namespaces here" is not something a
// daemon should assume about the machine it woke up on.
//
// On Linux, unprivileged, with nothing but the kernel (which is what a
// self-hoster's box is):
//
//   - **Network** — an empty network namespace. The app process has no route to
//     anything at all, so the manifest's host allowlist cannot be bypassed by
//     ignoring it: [Fetcher] runs the request on the app's behalf in relayd and
//     checks [Guard] first. Egress is not "default-deny by policy", it is
//     default-deny because there is no interface. Enforced.
//   - **Processes** — its own PID, IPC and UTS namespaces, its own process
//     group, `Pdeathsig`, and a uid mapped to nobody. It cannot see, signal or
//     inherit anything of relayd's. Enforced.
//   - **Filesystem** — Node's permission model (`--permission`), which denies
//     `fs`, `child_process`, native addons and worker threads outright, then
//     re-opens exactly two paths: the app's read-only root and its own writable
//     scratch. Enforced *by the runtime process*, which is a weaker claim than a
//     chroot and is reported as such — see [Enforcement.Filesystem].
//   - **CPU and file size** — `RLIMIT_CPU` and `RLIMIT_FSIZE` via `prlimit64`,
//     applied to the child before it is told to load any app code. Enforced.
//   - **Memory** — two halves, because one number cannot honestly cover it.
//     `--max-old-space-size` caps the JS heap (V8 enforces it); `RLIMIT_AS` caps
//     the whole address space (the kernel enforces it) with a floor, because V8
//     cannot start at all below roughly a gigabyte of *virtual* reservation. Both
//     numbers are reported. Neither is described as "the memory cap".
//   - **Wall clock** — the supervisor's, not the kernel's: SIGTERM to the process
//     group, a grace period, then SIGKILL. Enforced.
//
// Where a control cannot be had, it is not silently downgraded. [Runtime] refuses
// to start an app that holds a scope which can read the user's life — memory,
// audio, camera, the agent — on a sandbox that cannot enforce network isolation,
// because §3 says in one line why: *an app with `memory.read` and unrestricted
// network access is an exfiltration tool.* See [ErrCannotContain].
//
// # The SDK is the only interface, and it is minted per app
//
// There is no filesystem, no `child_process`, no raw socket, and no ambient
// global that reaches the host. Everything an app can do arrives as a property
// on `ctx`, and the property **does not exist** unless the manifest asked for the
// matching scope and the user granted it. Not present-and-refusing: absent. An
// app cannot feature-detect its way to a capability it was not given, and a
// permission sheet that says "this app cannot use your camera" is describing the
// object the app will actually receive.
//
// That is enforced twice on purpose. [Capabilities] decides what the runner
// builds, and [Host] holds a dispatch table containing only the granted
// methods — so a runner that was tampered with still cannot call one, and the
// answer it gets is "no such capability" rather than "denied", because from
// outside the grant there is nothing there to deny.
//
// # There is no scope for recording without indication
//
// §3 ends with a sentence that is not a policy line: *there is no "record without
// indication" scope, and there never will be. The LEDs are wired to capture and
// apps cannot address them.* Three things make that structural here rather than
// aspirational:
//
//   - [Scopes] is the closed vocabulary and it is the only source of scopes. A
//     capability that is not in [capabilities] cannot be minted, and every entry
//     in that table names the scope it needs. There is no device-control method,
//     no indicator method, and TestNoCapabilityCanAddressTheIndicators enumerates
//     the table and fails if one is ever added.
//   - [NewGlasses] refuses to build without an [Indicator]. A camera capability
//     that could exist without the thing that lights the LED is the bug; making
//     it unconstructable is the fix.
//   - [Glasses.Capture] calls the indicator *before* the capture and returns the
//     indicator's error without capturing. An app cannot ask for a silent still
//     because the only path to a still runs through the indication.
//
// # Secrets before anything is written, once more
//
// The rule `internal/index` and `internal/episode` already enforce holds here
// too, and for the same reason: an embedded key cannot be unembedded. [NewMemory]
// refuses to build without a redactor, an app's note goes through the detector
// before it reaches a store, and so does every line the app writes to its log —
// an app that reads a credential out of a transcript and prints it must not turn
// relayd's log into the place that credential ends up.
//
// # Memory access is scoped and logged
//
// Every read an app makes is recorded through [AccessLog] before the data is
// returned, and a read that could not be recorded **does not happen**. That is
// `internal/audit`'s rule (a failed append is a failed mutation) applied to the
// other direction: the point of the log is to make "this app read your whole
// archive on install" visible, and a log that drops writes when it is
// inconvenient cannot show that.
package apps
