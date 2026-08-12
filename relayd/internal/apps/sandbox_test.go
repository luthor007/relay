package apps

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWeakestIsTheWorstBoundary(t *testing.T) {
	e := Enforcement{
		Filesystem: Enforced("a", "n"),
		Network:    Enforced("a", "n"),
		Processes:  Partial("a", "n"),
		User:       Enforced("a", "n"),
		CPU:        Enforced("a", "n"),
		Memory:     Partial("a", "n"),
		WallClock:  Enforced("a", "n"),
		FileSize:   Enforced("a", "n"),
	}
	if e.Weakest() != ControlPartial {
		t.Errorf("weakest = %s, want partial", e.Weakest())
	}
	e.Network = Declared("nothing stops it")
	if e.Weakest() != ControlDeclared {
		t.Errorf("weakest = %s, want declared — a sandbox is as strong as its worst boundary", e.Weakest())
	}
	if got := e.Declares(); len(got) != 1 || got[0] != "network" {
		t.Errorf("declares = %v", got)
	}
}

func TestEveryControlIsNamedAndOrdered(t *testing.T) {
	var e Enforcement
	names := []string{}
	for _, c := range e.Controls() {
		names = append(names, c.Name)
	}
	want := "filesystem,network,processes,user,cpu,memory,wall-clock,file-size"
	if strings.Join(names, ",") != want {
		t.Errorf("controls = %v, want %s", names, want)
	}
	// The zero value is declared everywhere, which is the honest zero: a report
	// nobody filled in claims nothing.
	if e.Weakest() != ControlDeclared {
		t.Errorf("the zero Enforcement claims %s", e.Weakest())
	}
}

func TestADeclaredGuaranteeCannotNameAMechanism(t *testing.T) {
	g := Declared("nothing stops it")
	if g.By != "" {
		t.Error("Declared must not carry a mechanism; that is what makes the report readable")
	}
	if !strings.Contains(g.String(), "declared") {
		t.Errorf("%q", g.String())
	}
	if !strings.Contains(Enforced("kernel", "everything").String(), "by kernel") {
		t.Error("an enforced guarantee reads with its mechanism")
	}
}

func TestTheSandboxIsProbedRatherThanAssumed(t *testing.T) {
	node := requireNode(t)
	sb, err := NewSandbox(context.Background(), SandboxOptions{Probe: node})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name() == "" {
		t.Error("the implementation has to name itself, for the console and for the record")
	}
	e := sb.Enforcement()
	// The sandbox layer never claims the two boundaries it does not hold.
	if e.Filesystem.Control != ControlDeclared || e.WallClock.Control != ControlDeclared {
		t.Errorf("the bare sandbox claims a filesystem or clock boundary it does not have: %+v", e)
	}
	t.Logf("sandbox=%s network=%s processes=%s user=%s",
		sb.Name(), e.Network.Control, e.Processes.Control, e.User.Control)
}

func TestDisablingTheSandboxDowngradesHonestly(t *testing.T) {
	node := requireNode(t)
	sb, err := NewSandbox(context.Background(), SandboxOptions{Probe: node, Disable: true})
	if err != nil {
		t.Fatal(err)
	}
	e := sb.Enforcement()
	if e.Network.Control == ControlEnforced {
		t.Error("a disabled sandbox cannot enforce the network boundary")
	}
	if e.Network.By != "" {
		t.Errorf("and it must not name a mechanism: %q", e.Network.By)
	}
}

func TestASandboxNeedsSomethingToProbeWith(t *testing.T) {
	if _, err := NewSandbox(context.Background(), SandboxOptions{}); err == nil {
		t.Error("measuring with nothing is assuming")
	}
}

func TestLimitDefaultsAndFloors(t *testing.T) {
	l := Limits{CPUTime: 10 * time.Millisecond}.withDefaults()
	if l.CPUTime != time.Second {
		t.Errorf("RLIMIT_CPU counts whole seconds; rounding to zero would kill every app instantly: %s", l.CPUTime)
	}
	if l.AddressSpace != 0 {
		t.Errorf("RLIMIT_AS is opt-in, and the default must stay zero: %d", l.AddressSpace)
	}
	if as, raised := (Limits{AddressSpace: 1 << 30}).AddressSpaceRaised(); !raised || as != MinAddressSpace {
		t.Errorf("a limit under the floor must be raised and reported: %d %v", as, raised)
	}
	if as, raised := (Limits{}).AddressSpaceRaised(); as != 0 || raised {
		t.Errorf("zero means not applied: %d %v", as, raised)
	}
	if got := (Limits{Memory: 1}).HeapMB(); got != 16 {
		t.Errorf("a heap cap V8 cannot honour is a heap cap that crashes every app: %d", got)
	}
	if got := (Limits{Memory: 512 << 20}).HeapMB(); got != 512 {
		t.Errorf("heap = %d MiB", got)
	}
}

func TestTheLimitReportMatchesWhatWasAsked(t *testing.T) {
	rep := limitExpectation(Limits{Memory: 128 << 20})
	if rep.Memory.Control == ControlEnforced {
		t.Error("memory is never fully enforced; see the measurements in limits.go")
	}
	if !strings.Contains(rep.Memory.Note, "128 MiB") {
		t.Errorf("the note has to carry the number that binds: %q", rep.Memory.Note)
	}
}
