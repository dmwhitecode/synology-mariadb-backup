package schedule

import (
	"testing"
	"time"
)

func TestCronExpr(t *testing.T) {
	cases := []struct {
		s    Schedule
		want string
	}{
		{Schedule{Mode: Hourly}, "0 * * * *"},
		{Schedule{Mode: EveryNHours, EveryHours: 6}, "0 */6 * * *"},
		{Schedule{Mode: Daily, Time: "02:30"}, "30 2 * * *"},
		{Schedule{Mode: Daily, Time: "23:05"}, "5 23 * * *"},
		{Schedule{Mode: Weekly, Time: "04:00", Days: []int{5, 1, 1}}, "0 4 * * 1,5"},
		{Schedule{Mode: Cron, Cron: "  15   3 * * 1-5 "}, "15 3 * * 1-5"},
	}
	for _, c := range cases {
		got, err := c.s.CronExpr()
		if err != nil || got != c.want {
			t.Errorf("%+v -> %q, %v; want %q", c.s, got, err, c.want)
		}
	}
}

func TestCronExprErrors(t *testing.T) {
	bad := []Schedule{
		{Mode: ""},
		{Mode: "fortnightly"},
		{Mode: EveryNHours, EveryHours: 0},
		{Mode: EveryNHours, EveryHours: 24},
		{Mode: Daily, Time: "2:30pm"},
		{Mode: Daily, Time: "25:00"},
		{Mode: Daily, Time: ""},
		{Mode: Weekly, Time: "04:00"},
		{Mode: Weekly, Time: "04:00", Days: []int{7}},
		{Mode: Cron, Cron: ""},
		{Mode: Cron, Cron: "* * *"},
		{Mode: Cron, Cron: "0 0 * * * *"},
	}
	for _, s := range bad {
		if _, err := s.CronExpr(); err == nil {
			t.Errorf("%+v should be invalid", s)
		}
	}
}

func TestNext(t *testing.T) {
	from := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC) // a Wednesday
	s := Schedule{Mode: Weekly, Time: "09:00", Days: []int{1}}
	next, err := s.Next(from)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestDescribe(t *testing.T) {
	if d := (Schedule{Mode: Weekly, Time: "01:00", Days: []int{0, 6}}).Describe(); d != "Sun, Sat at 01:00" {
		t.Fatalf("got %q", d)
	}
	if d := Default().Describe(); d != "Daily at 02:00" {
		t.Fatalf("got %q", d)
	}
}
