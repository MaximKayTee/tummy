package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseSchedule parses a 5-field cron expression, @hourly/@daily/... or @every 15m.
// Cron fields are UTC unless the expression starts with CRON_TZ=Area/City.
func ParseSchedule(s string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(s)
	if err != nil {
		hint := ""
		if strings.HasPrefix(strings.TrimSpace(s), "every ") {
			hint = " (@" + strings.TrimSpace(s) + "?)"
		}
		return nil, fmt.Errorf("schedule %q: %v%s", s, err, hint)
	}
	return sched, nil
}

// NextRun is the first fire time of sched after t, evaluated in UTC unless the
// schedule names its own zone.
func NextRun(sched cron.Schedule, t time.Time) time.Time {
	return sched.Next(t.UTC())
}
