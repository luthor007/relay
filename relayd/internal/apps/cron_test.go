package apps

import (
	"testing"
	"time"
)

func TestCronRefusesWhatItCannotHonour(t *testing.T) {
	for _, expr := range []string{
		"", "every morning", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 32 * *", "* * * 13 *", "* * * * 7", "*/0 * * * *", "5-1 * * * *", "a * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("%q should not parse", expr)
		}
	}
}

func TestCronMatches(t *testing.T) {
	cases := []struct {
		expr string
		when string
		want bool
	}{
		{"0 8 * * *", "2026-08-10T08:00:00Z", true},
		{"0 8 * * *", "2026-08-10T08:01:00Z", false},
		{"*/15 * * * *", "2026-08-10T09:30:00Z", true},
		{"*/15 * * * *", "2026-08-10T09:31:00Z", false},
		{"0 22 * * mon-fri", "2026-08-10T22:00:00Z", true}, // a Monday
		{"0 22 * * mon-fri", "2026-08-08T22:00:00Z", false},
		{"0 0 1 jan *", "2027-01-01T00:00:00Z", true},
		{"30 7,19 * * *", "2026-08-10T19:30:00Z", true},
		{"30 7,19 * * *", "2026-08-10T18:30:00Z", false},
	}
	for _, tc := range cases {
		c, err := ParseCron(tc.expr)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		when, _ := time.Parse(time.RFC3339, tc.when)
		if got := c.Matches(when); got != tc.want {
			t.Errorf("%q at %s = %v, want %v", tc.expr, tc.when, got, tc.want)
		}
	}
}

func TestCronUnionsDayOfMonthAndDayOfWeek(t *testing.T) {
	// Cron's oldest wart, and one a naive scheduler gets wrong: when both day
	// fields are restricted the match is their union, not their intersection.
	c, err := ParseCron("0 0 1 * mon")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		when string
		want bool
	}{
		{"2026-09-01T00:00:00Z", true},  // the 1st, a Tuesday
		{"2026-09-07T00:00:00Z", true},  // a Monday, not the 1st
		{"2026-09-08T00:00:00Z", false}, // neither
	} {
		when, _ := time.Parse(time.RFC3339, tc.when)
		if got := c.Matches(when); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.when, got, tc.want)
		}
	}
}

func TestCronNextIsInTheUsersTimezone(t *testing.T) {
	// APP-PLATFORM.md §4 says "in the user's timezone" and means it: a cron
	// expression interpreted in UTC fires a "every morning at 8" app at midnight
	// for half the year.
	montreal, err := time.LoadLocation("America/Montreal")
	if err != nil {
		t.Skipf("no tzdata here: %v", err)
	}
	c, err := ParseCron("0 8 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	next := c.Next(from, montreal)
	if next.IsZero() {
		t.Fatal("no next firing")
	}
	if h, _, _ := next.In(montreal).Clock(); h != 8 {
		t.Errorf("next = %s, want 08:00 local", next.In(montreal))
	}
	if next.UTC().Hour() == 8 {
		t.Error("the expression was read in UTC")
	}
}

func TestCronNextSkipsImpossibleDates(t *testing.T) {
	c, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if next := c.Next(time.Now(), time.UTC); !next.IsZero() {
		t.Errorf("30 February should never fire, got %s", next)
	}
}

func TestCronNextIsStrictlyAfter(t *testing.T) {
	c, _ := ParseCron("*/5 * * * *")
	from := time.Date(2026, 8, 10, 9, 5, 0, 0, time.UTC)
	next := c.Next(from, time.UTC)
	if !next.After(from) {
		t.Errorf("next %s is not after %s; a scheduler that returns its own instant fires twice", next, from)
	}
	if next.Minute() != 10 {
		t.Errorf("next = %s", next)
	}
}
