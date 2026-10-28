// Package schedule converts user-friendly schedule settings into cron
// expressions and describes them for display.
package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Mode selects how the schedule is expressed.
type Mode string

const (
	Hourly      Mode = "hourly"
	EveryNHours Mode = "every_n_hours"
	Daily       Mode = "daily"
	Weekly      Mode = "weekly"
	Cron        Mode = "cron"
)

// Modes lists every mode in display order.
var Modes = []Mode{Hourly, EveryNHours, Daily, Weekly, Cron}

// Schedule is a user-level description of when backups run.
type Schedule struct {
	Mode Mode `json:"mode"`
	// EveryHours is used by EveryNHours (1..23).
	EveryHours int `json:"every_hours,omitempty"`
	// Time is "HH:MM" in 24-hour local time, used by Daily and Weekly.
	Time string `json:"time,omitempty"`
	// Days are weekdays 0 (Sunday) to 6 (Saturday), used by Weekly.
	Days []int `json:"days,omitempty"`
	// Cron is a raw five-field expression, used by Cron.
	Cron string `json:"cron,omitempty"`
}

// Parser is the standard five-field cron parser shared with the scheduler.
var Parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var dayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// Default is a daily backup at 02:00.
func Default() Schedule { return Schedule{Mode: Daily, Time: "02:00"} }

// Validate reports whether the schedule can be turned into a cron expression.
func (s Schedule) Validate() error {
	_, err := s.CronExpr()
	return err
}

// CronExpr returns the five-field cron expression for the schedule.
func (s Schedule) CronExpr() (string, error) {
	switch s.Mode {
	case Hourly:
		return "0 * * * *", nil
	case EveryNHours:
		if s.EveryHours < 1 || s.EveryHours > 23 {
			return "", errors.New("every N hours must be between 1 and 23")
		}
		return fmt.Sprintf("0 */%d * * *", s.EveryHours), nil
	case Daily:
		h, m, err := parseClock(s.Time)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d * * *", m, h), nil
	case Weekly:
		h, m, err := parseClock(s.Time)
		if err != nil {
			return "", err
		}
		days, err := normaliseDays(s.Days)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d * * %s", m, h, joinInts(days)), nil
	case Cron:
		expr := strings.Join(strings.Fields(s.Cron), " ")
		if expr == "" {
			return "", errors.New("cron expression is required")
		}
		if _, err := Parser.Parse(expr); err != nil {
			return "", fmt.Errorf("invalid cron expression: %w", err)
		}
		return expr, nil
	}
	return "", fmt.Errorf("unknown schedule mode %q", s.Mode)
}

// Next returns the next run time after from, in from's location.
func (s Schedule) Next(from time.Time) (time.Time, error) {
	expr, err := s.CronExpr()
	if err != nil {
		return time.Time{}, err
	}
	sched, err := Parser.Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}

// Describe returns a short human-readable summary.
func (s Schedule) Describe() string {
	switch s.Mode {
	case Hourly:
		return "Every hour"
	case EveryNHours:
		if s.EveryHours == 1 {
			return "Every hour"
		}
		return fmt.Sprintf("Every %d hours", s.EveryHours)
	case Daily:
		return "Daily at " + s.Time
	case Weekly:
		days, err := normaliseDays(s.Days)
		if err != nil {
			return "Weekly (invalid days)"
		}
		names := make([]string, len(days))
		for i, d := range days {
			names[i] = dayNames[d]
		}
		return fmt.Sprintf("%s at %s", strings.Join(names, ", "), s.Time)
	case Cron:
		return "Cron: " + strings.TrimSpace(s.Cron)
	}
	return "Not scheduled"
}

// HasDay reports whether weekday d is selected (template helper).
func (s Schedule) HasDay(d int) bool {
	for _, x := range s.Days {
		if x == d {
			return true
		}
	}
	return false
}

func parseClock(v string) (hour, minute int, err error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("time must be HH:MM")
	}
	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, errors.New("time must be HH:MM in 24-hour format")
	}
	return hour, minute, nil
}

func normaliseDays(days []int) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, d := range days {
		if d < 0 || d > 6 {
			return nil, errors.New("weekday must be between 0 (Sunday) and 6 (Saturday)")
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("select at least one weekday")
	}
	sort.Ints(out)
	return out, nil
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
