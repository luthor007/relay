package bus

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QuietHours is the window in which Relay does not speak.
//
// ADAPTERS.md §7: "Quiet hours apply to completions only. A blocked session at
// 3 a.m. is still blocked at 8 a.m.; holding the ping just means eight wasted
// hours. Hold the *speech*, keep the phone notification silent-but-present."
//
// So this type answers exactly one question — is it quiet right now — and the
// Pinger applies it to informational pings and to nothing else. The zero value
// is disabled.
type QuietHours struct {
	// Start and End are local wall-clock minutes since midnight. A window that
	// wraps past midnight (22:00 → 07:00) is the normal case, not the edge one.
	Start, End int
	// Loc is the location the wall clock is read in. Nil means time.Local.
	Loc *time.Location

	set bool
}

// ParseQuietHours reads "22:00-07:00". An empty string disables quiet hours,
// which is the default: a device that silently stops talking because of a
// setting nobody remembers is worse than one that occasionally speaks late.
func ParseQuietHours(s string) (QuietHours, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return QuietHours{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return QuietHours{}, fmt.Errorf("bus: quiet hours %q is not START-END, e.g. 22:00-07:00", s)
	}
	start, err := parseClock(a)
	if err != nil {
		return QuietHours{}, fmt.Errorf("bus: quiet hours %q: %w", s, err)
	}
	end, err := parseClock(b)
	if err != nil {
		return QuietHours{}, fmt.Errorf("bus: quiet hours %q: %w", s, err)
	}
	if start == end {
		return QuietHours{}, fmt.Errorf("bus: quiet hours %q starts and ends at the same minute", s)
	}
	return QuietHours{Start: start, End: end, set: true}, nil
}

func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("bus: %q is not HH:MM", s)
	}
	hi, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hi < 0 || hi > 23 {
		return 0, fmt.Errorf("bus: %q is not a valid hour", s)
	}
	mi, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || mi < 0 || mi > 59 {
		return 0, fmt.Errorf("bus: %q is not a valid minute", s)
	}
	return hi*60 + mi, nil
}

// Enabled reports whether quiet hours are configured at all.
func (q QuietHours) Enabled() bool { return q.set }

// Active reports whether t falls inside the quiet window.
func (q QuietHours) Active(t time.Time) bool {
	if !q.set {
		return false
	}
	loc := q.Loc
	if loc == nil {
		loc = time.Local
	}
	lt := t.In(loc)
	min := lt.Hour()*60 + lt.Minute()
	if q.Start < q.End {
		return min >= q.Start && min < q.End
	}
	// Wraps midnight.
	return min >= q.Start || min < q.End
}

// String renders the window back as "22:00-07:00", or "" when disabled.
func (q QuietHours) String() string {
	if !q.set {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", q.Start/60, q.Start%60, q.End/60, q.End%60)
}
