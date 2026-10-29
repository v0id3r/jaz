package usage

import (
	"errors"
	"time"
)

const (
	DateLayout       = "2006-01-02"
	DefaultDailyDays = 30
	MaxDailyDays     = 365
)

var (
	ErrInvalidDays  = errors.New("days must be a positive integer")
	ErrInvalidRange = errors.New("provide both start and end, in order, spanning at most 365 days, without days")
)

type DailyQuery struct {
	Days     int
	Range    *DateRange
	Location *time.Location
}

// DateRange contains inclusive calendar dates; their clock and timezone are ignored.
type DateRange struct {
	Start, End time.Time
}

type dailyWindow struct {
	first, end time.Time
	location   *time.Location
}

func (q DailyQuery) window(now time.Time) (dailyWindow, error) {
	loc := q.Location
	if loc == nil {
		loc = time.Local
	}
	if q.Range != nil {
		if q.Days != 0 {
			return dailyWindow{}, ErrInvalidRange
		}
		start := calendarDate(q.Range.Start)
		end := calendarDate(q.Range.End).AddDate(0, 0, 1)
		if !end.After(start) || end.After(start.AddDate(0, 0, MaxDailyDays)) {
			return dailyWindow{}, ErrInvalidRange
		}
		return dailyWindow{start, end, loc}, nil
	}
	if q.Days < 0 {
		return dailyWindow{}, ErrInvalidDays
	}
	days := min(q.Days, MaxDailyDays)
	if days == 0 {
		days = DefaultDailyDays
	}
	end := calendarDate(now.In(loc)).AddDate(0, 0, 1)
	return dailyWindow{end.AddDate(0, 0, -days), end, loc}, nil
}

// Calendar arithmetic uses UTC to preserve dates even when a zone skips midnight or an entire day.
func calendarDate(date time.Time) time.Time {
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
}

func (w dailyWindow) bounds() (time.Time, time.Time) {
	return dayBoundary(w.first, w.location), dayBoundary(w.end, w.location)
}

// Find the first instant on or after this civil date across skipped or repeated midnights.
func dayBoundary(date time.Time, loc *time.Location) time.Time {
	probe := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc).Add(-24 * time.Hour)
	for {
		_, offset := probe.Zone()
		start, end := probe.ZoneBounds()
		candidate := date.Add(-time.Duration(offset) * time.Second)
		if _, previous := start.Add(-time.Second).Zone(); !start.IsZero() && candidate.Before(start) && previous != offset {
			candidate = start
		}
		if end.IsZero() || candidate.Before(end) {
			return candidate
		}
		// Past a zone's last listed transition Go derives bounds from its rule:
		// they split each UTC year without a change of offset, and leap years end
		// a day early, so a bound may not even lie after probe.
		if end.After(probe) {
			probe = end
		} else {
			probe = probe.Add(time.Hour)
		}
	}
}
