/**
 * SPDX-FileComment: MitM Maintenance Monitor - cron expressions
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file cron.go
 * @brief Cron expressions with the semantics of the Rust crate `cron` 0.12 used by
 *        mitm_scheduler-server, to compute when a scheduled program should have run.
 * @version 0.2.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 *
 * Semantics (cron 0.12.1, as evaluated by the scheduler in UTC):
 *   - fields: seconds minutes hours day-of-month month day-of-week [year];
 *     the scheduler turns a 5-field expression into "0 <expr> *"
 *   - shorthands @yearly, @monthly, @weekly, @daily, @hourly
 *   - every field is a comma list of `*`, `a`, `a-b`, `NAME`, `NAME-NAME` or `<spec>/<step>`;
 *     `a/step` means a..max; `?` is allowed for day-of-month and day-of-week only
 *   - day-of-week 1-7 with Sunday = 1 (names sun..sat); months 1-12 (names jan..dec)
 *   - day-of-month AND day-of-week must both match (no Vixie-cron OR)
 */

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronField is the set of allowed values of one field.
type cronField map[int]bool

// cronSchedule is a parsed cron expression.
type cronSchedule struct {
	seconds, minutes, hours, daysOfMonth, months, daysOfWeek, years cronField
}

type cronFieldSpec struct {
	name     string
	min, max int
	names    map[string]int
	allowAny bool // `?`
}

var (
	monthNames = map[string]int{
		"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
		"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8,
		"sep": 9, "september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
	}
	weekdayNames = map[string]int{
		"sun": 1, "sunday": 1, "mon": 2, "monday": 2, "tue": 3, "tues": 3, "tuesday": 3,
		"wed": 4, "wednesday": 4, "thu": 5, "thurs": 5, "thursday": 5, "fri": 6, "friday": 6, "sat": 7, "saturday": 7,
	}
	cronSpecs = []cronFieldSpec{
		{name: "seconds", min: 0, max: 59},
		{name: "minutes", min: 0, max: 59},
		{name: "hours", min: 0, max: 23},
		{name: "days of month", min: 1, max: 31, allowAny: true},
		{name: "months", min: 1, max: 12, names: monthNames},
		{name: "days of week", min: 1, max: 7, names: weekdayNames, allowAny: true},
		{name: "years", min: 1970, max: 2100},
	}
)

// parseSchedulerCron parses a scheduled_programs.cron_expr exactly as the scheduler does.
func parseSchedulerCron(expr string) (*cronSchedule, error) {
	if len(strings.Fields(expr)) == 5 {
		expr = "0 " + expr + " *"
	}
	return parseCron(expr)
}

func parseCron(expr string) (*cronSchedule, error) {
	expr = strings.TrimSpace(expr)
	switch expr {
	case "@yearly":
		return parseCron("0 0 0 1 1 * *")
	case "@monthly":
		return parseCron("0 0 0 1 * * *")
	case "@weekly":
		return parseCron("0 0 0 * * 1 *")
	case "@daily":
		return parseCron("0 0 0 * * * *")
	case "@hourly":
		return parseCron("0 0 * * * * *")
	}
	parts := strings.Fields(expr)
	if len(parts) == 6 {
		parts = append(parts, "*")
	}
	if len(parts) != 7 {
		return nil, fmt.Errorf("cron expression %q must have 5, 6 or 7 fields", expr)
	}
	fields := make([]cronField, len(parts))
	for i, part := range parts {
		field, err := parseCronField(part, cronSpecs[i])
		if err != nil {
			return nil, fmt.Errorf("cron expression %q: %w", expr, err)
		}
		fields[i] = field
	}
	return &cronSchedule{fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6]}, nil
}

func parseCronField(text string, spec cronFieldSpec) (cronField, error) {
	field := cronField{}
	for _, item := range strings.Split(text, ",") {
		values, err := parseCronItem(item, spec)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			if value < spec.min || value > spec.max {
				return nil, fmt.Errorf("%s must be between %d and %d (%d specified)", spec.name, spec.min, spec.max, value)
			}
			field[value] = true
		}
	}
	return field, nil
}

// parseCronItem returns the values of one comma-separated item.
func parseCronItem(item string, spec cronFieldSpec) ([]int, error) {
	if item == "" {
		return nil, fmt.Errorf("empty item in %s", spec.name)
	}
	base, stepText, hasStep := strings.Cut(item, "/")
	if !hasStep {
		if name, ok := lookupName(item, spec); ok { // single name (NamedPoint)
			return []int{name}, nil
		}
		return parseCronSpecifier(item, spec)
	}

	step, err := strconv.Atoi(stepText)
	if err != nil || step < 0 {
		return nil, fmt.Errorf("invalid step %q in %s", stepText, spec.name)
	}
	if step == 0 {
		return nil, errors.New("range step cannot be zero")
	}
	var values []int
	if n, err := strconv.Atoi(base); err == nil { // a/step: a..max
		if n < spec.min || n > spec.max {
			return nil, fmt.Errorf("%s must be between %d and %d (%d specified)", spec.name, spec.min, spec.max, n)
		}
		for v := n; v <= spec.max; v++ {
			values = append(values, v)
		}
	} else {
		if values, err = parseCronSpecifier(base, spec); err != nil {
			return nil, err
		}
	}
	var stepped []int
	for i := 0; i < len(values); i += step {
		stepped = append(stepped, values[i])
	}
	return stepped, nil
}

// parseCronSpecifier handles `*`, `?`, `a`, `a-b` and `NAME-NAME`; the result is sorted.
func parseCronSpecifier(text string, spec cronFieldSpec) ([]int, error) {
	if text == "*" || (text == "?" && spec.allowAny) {
		return rangeValues(spec.min, spec.max), nil
	}
	if n, err := strconv.Atoi(text); err == nil {
		return []int{n}, nil
	}
	if startText, endText, ok := strings.Cut(text, "-"); ok {
		start, startErr := strconv.Atoi(startText)
		end, endErr := strconv.Atoi(endText)
		if startErr != nil || endErr != nil {
			var okStart, okEnd bool
			start, okStart = lookupName(startText, spec)
			end, okEnd = lookupName(endText, spec)
			if !okStart || !okEnd {
				return nil, fmt.Errorf("invalid range %q in %s", text, spec.name)
			}
		}
		if start < spec.min || end > spec.max || start > end {
			return nil, fmt.Errorf("invalid range for %s: %s", spec.name, text)
		}
		return rangeValues(start, end), nil
	}
	return nil, fmt.Errorf("invalid value %q in %s", text, spec.name)
}

func lookupName(text string, spec cronFieldSpec) (int, bool) {
	if spec.names == nil {
		return 0, false
	}
	value, ok := spec.names[strings.ToLower(text)]
	return value, ok
}

func rangeValues(start, end int) []int {
	values := make([]int, 0, end-start+1)
	for v := start; v <= end; v++ {
		values = append(values, v)
	}
	return values
}

// matchesMinute reports whether the schedule fires in the minute of t (any second), and the first second.
func (s *cronSchedule) matchesMinute(t time.Time) (int, bool) {
	t = t.UTC()
	if !s.years[t.Year()] || !s.months[int(t.Month())] || !s.daysOfMonth[t.Day()] ||
		!s.daysOfWeek[int(t.Weekday())+1] || !s.hours[t.Hour()] || !s.minutes[t.Minute()] {
		return 0, false
	}
	for second := 0; second <= 59; second++ {
		if s.seconds[second] {
			return second, true
		}
	}
	return 0, false
}

// fireTimes returns the first fire time of every matching minute in (from, to], at most max entries.
// Several fire times within one minute count once: the scheduler checks its programs once a minute.
func (s *cronSchedule) fireTimes(from, to time.Time, max int) []time.Time {
	var times []time.Time
	minute := from.UTC().Truncate(time.Minute)
	for ; !minute.After(to) && len(times) < max; minute = minute.Add(time.Minute) {
		second, ok := s.matchesMinute(minute)
		if !ok {
			continue
		}
		fire := minute.Add(time.Duration(second) * time.Second)
		if fire.After(from) && !fire.After(to) {
			times = append(times, fire)
		}
	}
	return times
}
