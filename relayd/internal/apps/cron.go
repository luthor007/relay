package apps

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A five-field cron parser, in the user's timezone — APP-PLATFORM.md §4's
// `schedule` trigger and nothing more.
//
// It is here rather than behind a dependency for the reason SYSTEM.md §8 gives
// about the binary: `relayd` is cgo-free and cross-compiles to four targets from
// one build, and every dependency is a chance to lose that. Five fields, five
// operators, no seconds and no `@yearly`: an expression this does not
// understand is refused at install with the reason, which is better than
// silently accepting a schedule that never fires.

// Cron is a parsed five-field expression: minute hour day-of-month month
// day-of-week.
type Cron struct {
	expr   string
	minute bitset
	hour   bitset
	dom    bitset
	month  bitset
	dow    bitset
	// domRestricted and dowRestricted record whether the field was `*`. Cron's
	// oldest wart: when both day-of-month and day-of-week are restricted the
	// match is their union, not their intersection, and a scheduler that gets
	// this wrong fires a Monday-the-1st job every Monday.
	domRestricted bool
	dowRestricted bool
}

type bitset uint64

func (b bitset) has(i int) bool { return b&(1<<uint(i)) != 0 }

// String returns the expression as written.
func (c Cron) String() string { return c.expr }

// ParseCron parses a five-field expression.
func ParseCron(expr string) (Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("cron %q needs five fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	c := Cron{expr: strings.Join(fields, " ")}
	var err error
	if c.minute, err = parseField(fields[0], 0, 59, nil); err != nil {
		return Cron{}, fmt.Errorf("cron minute: %w", err)
	}
	if c.hour, err = parseField(fields[1], 0, 23, nil); err != nil {
		return Cron{}, fmt.Errorf("cron hour: %w", err)
	}
	if c.dom, err = parseField(fields[2], 1, 31, nil); err != nil {
		return Cron{}, fmt.Errorf("cron day-of-month: %w", err)
	}
	if c.month, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return Cron{}, fmt.Errorf("cron month: %w", err)
	}
	if c.dow, err = parseField(fields[4], 0, 6, dayNames); err != nil {
		return Cron{}, fmt.Errorf("cron day-of-week: %w", err)
	}
	c.domRestricted = fields[2] != "*"
	c.dowRestricted = fields[4] != "*"
	return c, nil
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

func parseField(f string, min, max int, names map[string]int) (bitset, error) {
	var out bitset
	for _, part := range strings.Split(f, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("empty element in %q", f)
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			var err error
			step, err = strconv.Atoi(part[i+1:])
			if err != nil || step <= 0 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			part = part[:i]
		}
		lo, hi := min, max
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			var err error
			if lo, err = parseValue(bits[0], names); err != nil {
				return 0, err
			}
			if hi, err = parseValue(bits[1], names); err != nil {
				return 0, err
			}
		default:
			v, err := parseValue(part, names)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
			// `7/2` without a range means "from 7 to the end of the field",
			// which is the behaviour every cron implements.
			if step > 1 {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("%q is outside %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			out |= 1 << uint(v)
		}
	}
	if out == 0 {
		return 0, fmt.Errorf("%q matches nothing", f)
	}
	return out, nil
}

func parseValue(s string, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

// Matches reports whether t is a firing minute, in t's own location.
func (c Cron) Matches(t time.Time) bool {
	if !c.minute.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) {
		return false
	}
	dom := c.dom.has(t.Day())
	dow := c.dow.has(int(t.Weekday()))
	switch {
	case c.domRestricted && c.dowRestricted:
		return dom || dow
	case c.domRestricted:
		return dom
	case c.dowRestricted:
		return dow
	default:
		return true
	}
}

// Next is the first firing time strictly after t, in loc. It returns the zero
// time when nothing matches within four years, which for a five-field
// expression means an impossible date such as 30 February.
func (c Cron) Next(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	cur := t.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := cur.AddDate(4, 0, 0)
	for cur.Before(limit) {
		if !c.month.has(int(cur.Month())) {
			cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatches(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !c.hour.has(cur.Hour()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !c.minute.has(cur.Minute()) {
			cur = cur.Add(time.Minute)
			continue
		}
		return cur
	}
	return time.Time{}
}

func (c Cron) dayMatches(t time.Time) bool {
	dom := c.dom.has(t.Day())
	dow := c.dow.has(int(t.Weekday()))
	switch {
	case c.domRestricted && c.dowRestricted:
		return dom || dow
	case c.domRestricted:
		return dom
	case c.dowRestricted:
		return dow
	default:
		return true
	}
}
