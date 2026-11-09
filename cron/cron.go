// Package cron parses standard five-field cron expressions and computes
// their next run time in a time zone, for heartbeat checks whose jobs run on
// a cron schedule rather than at a fixed period.
package cron

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression in a time zone.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
	loc                           *time.Location
	expr                          string
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse reads "minute hour day-of-month month day-of-week" (or a macro such
// as @daily) in the given location; nil means UTC. As in Vixie cron, when
// both day fields are restricted a day matches if either matches.
func Parse(expr string, loc *time.Location) (*Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	s := &Schedule{loc: loc, expr: strings.TrimSpace(expr)}
	spec := s.expr
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q: want 5 fields (minute hour day month weekday), got %d", expr, len(fields))
	}
	var err error
	if s.minute, err = field(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	if s.hour, err = field(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	if s.dom, err = field(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron: day of month: %w", err)
	}
	if s.month, err = field(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	if s.dow, err = field(fields[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron: day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	s.domStar = strings.HasPrefix(fields[2], "*")
	s.dowStar = strings.HasPrefix(fields[4], "*")
	return s, nil
}

func field(f string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
		}
		var from, to int
		switch {
		case rng == "*":
			from, to = lo, hi
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = value(a, names); err != nil {
				return 0, err
			}
			if to, err = value(b, names); err != nil {
				return 0, err
			}
		default:
			v, err := value(rng, names)
			if err != nil {
				return 0, err
			}
			from, to = v, v
			if hasStep {
				to = hi // "5/15" means from 5 to the end, every 15
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set |= 1 << v
		}
	}
	if set == 0 {
		return 0, errors.New("empty field")
	}
	return set, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}

// String returns the expression as written.
func (s *Schedule) String() string { return s.expr }

// Location returns the schedule's time zone.
func (s *Schedule) Location() *time.Location { return s.loc }

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<t.Day()) != 0
	dow := s.dow&(1<<int(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// Next returns the first run time strictly after t, or the zero time if
// there is none within five years (e.g. "0 0 31 2 *"). A run that falls in
// an hour skipped by a daylight saving change happens right after the gap;
// in a repeated hour it happens at both occurrences.
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<int(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, s.loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.loc)
			continue
		}
		if s.hour&(1<<t.Hour()) == 0 {
			next := nextBit(s.hour, t.Hour())
			if next < 0 {
				t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.loc)
			} else {
				candidate := time.Date(t.Year(), t.Month(), t.Day(), next, 0, 0, 0, s.loc)
				if candidate.Hour() != next {
					// The hour is skipped by a daylight saving change: like
					// cron, run at the first instant after the gap.
					return time.Date(t.Year(), t.Month(), t.Day(), next+1, 0, 0, 0, s.loc)
				}
				t = candidate
			}
			continue
		}
		if s.minute&(1<<t.Minute()) == 0 {
			next := nextBit(s.minute, t.Minute())
			if next < 0 {
				t = t.Truncate(time.Hour).Add(time.Hour)
			} else {
				t = t.Add(time.Duration(next-t.Minute()) * time.Minute)
			}
			continue
		}
		return t
	}
	return time.Time{}
}

// nextBit returns the lowest set bit above from, or -1.
func nextBit(set uint64, from int) int {
	rest := set >> (from + 1) << (from + 1)
	if rest == 0 {
		return -1
	}
	return bits.TrailingZeros64(rest)
}
