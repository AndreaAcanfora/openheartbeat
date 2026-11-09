package cron

import (
	"testing"
	"time"
)

func must(t *testing.T, expr string, loc *time.Location) *Schedule {
	t.Helper()
	s, err := Parse(expr, loc)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct{ expr, after, want string }{
		{"* * * * *", "2026-11-09T10:15:30Z", "2026-11-09T10:16:00Z"},
		{"0 3 * * *", "2026-11-09T10:15:00Z", "2026-11-10T03:00:00Z"},
		{"0 3 * * *", "2026-11-09T02:59:59Z", "2026-11-09T03:00:00Z"},
		{"0 3 * * *", "2026-11-09T03:00:00Z", "2026-11-10T03:00:00Z"}, // strictly after
		{"*/15 * * * *", "2026-11-09T10:16:00Z", "2026-11-09T10:30:00Z"},
		{"5/20 * * * *", "2026-11-09T10:46:00Z", "2026-11-09T11:05:00Z"},
		{"0 9-17/4 * * *", "2026-11-09T10:00:00Z", "2026-11-09T13:00:00Z"},
		{"30 2 * * mon-fri", "2026-11-07T00:00:00Z", "2026-11-09T02:30:00Z"}, // Saturday → Monday
		{"0 0 * * 7", "2026-11-09T00:00:00Z", "2026-11-15T00:00:00Z"},        // 7 is Sunday
		{"0 0 1,15 * *", "2026-11-02T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"0 0 29 feb *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"0 0 1 jan-mar/2 *", "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"@daily", "2026-11-09T10:00:00Z", "2026-11-10T00:00:00Z"},
		{"@hourly", "2026-11-09T10:00:00Z", "2026-11-09T11:00:00Z"},
		{"@weekly", "2026-11-09T10:00:00Z", "2026-11-15T00:00:00Z"},
		// Both day fields restricted: either matches (Vixie cron).
		{"0 0 13 * fri", "2026-11-09T00:00:00Z", "2026-11-13T00:00:00Z"},
		{"0 0 20 * fri", "2026-11-09T00:00:00Z", "2026-11-13T00:00:00Z"},
	}
	for _, c := range cases {
		got := must(t, c.expr, nil).Next(at(c.after))
		if !got.Equal(at(c.want)) {
			t.Errorf("%q after %s = %s, want %s", c.expr, c.after, got.UTC().Format(time.RFC3339), c.want)
		}
	}
}

func TestTimeZones(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no time zone data")
	}
	s := must(t, "0 3 * * *", rome)
	// 03:00 in Rome is 02:00 UTC in winter and 01:00 UTC in summer.
	if got := s.Next(at("2026-11-09T12:00:00Z")); !got.Equal(at("2026-11-10T02:00:00Z")) {
		t.Errorf("winter: %s", got.UTC())
	}
	if got := s.Next(at("2026-07-09T12:00:00Z")); !got.Equal(at("2026-07-10T01:00:00Z")) {
		t.Errorf("summer: %s", got.UTC())
	}
	// 02:30 does not exist on 2026-03-29 in Rome (02:00 → 03:00): the run moves to 03:00.
	gap := must(t, "30 2 * * *", rome)
	if got := gap.Next(at("2026-03-28T12:00:00Z")); !got.Equal(at("2026-03-29T01:00:00Z")) {
		t.Errorf("spring forward: %s (%s)", got.UTC(), got)
	}
}

func TestNoRun(t *testing.T) {
	if got := must(t, "0 0 31 2 *", nil).Next(at("2026-01-01T00:00:00Z")); !got.IsZero() {
		t.Errorf("31 February: %s", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "x * * * *", "@sometimes"} {
		if _, err := Parse(expr, nil); err == nil {
			t.Errorf("Parse(%q) succeeded", expr)
		}
	}
	if s := must(t, "0 3 * * *", nil); s.String() != "0 3 * * *" || s.Location() != time.UTC {
		t.Error("String or Location")
	}
}

func TestRepeatedHour(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no time zone data")
	}
	// On 2026-10-25 Rome goes from 03:00 CEST back to 02:00 CET: 02:30 happens twice.
	s := must(t, "30 2 * * *", rome)
	first := s.Next(at("2026-10-25T00:00:00Z"))
	second := s.Next(first)
	if !first.Equal(at("2026-10-25T00:30:00Z")) || !second.Equal(at("2026-10-25T01:30:00Z")) {
		t.Errorf("repeated hour: %s, %s", first.UTC(), second.UTC())
	}
}
