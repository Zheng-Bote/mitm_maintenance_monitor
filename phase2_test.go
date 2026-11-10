// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func utc(value string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronSchedulerSemantics(t *testing.T) {
	for _, tc := range []struct {
		expr  string
		at    string
		match bool
	}{
		{"*/15 * * * *", "2026-11-10 10:30:00", true},
		{"*/15 * * * *", "2026-11-10 10:31:00", false},
		{"0 3 * * *", "2026-11-10 03:00:00", true},
		{"0 3 * * *", "2026-11-10 04:00:00", false},
		// Day of week 1-7 with Sunday = 1 (2026-11-08 is a Sunday, 2026-11-09 a Monday).
		{"0 2 * * 1", "2026-11-08 02:00:00", true},
		{"0 2 * * 1", "2026-11-09 02:00:00", false},
		{"0 2 * * 2-6", "2026-11-09 02:00:00", true},
		{"0 2 * * MON-FRI", "2026-11-09 02:00:00", true},
		{"0 2 * * sun", "2026-11-08 02:00:00", true},
		// Day of month AND day of week (2026-11-01 is a Sunday).
		{"0 0 1 * 2", "2026-11-01 00:00:00", false},
		{"0 0 1 * 1", "2026-11-01 00:00:00", true},
		{"0 0 ? * 1", "2026-11-08 00:00:00", true},
		// a/step means a..max.
		{"5/20 * * * *", "2026-11-10 10:45:00", true},
		{"5/20 * * * *", "2026-11-10 10:00:00", false},
		{"1-59/5 * * * *", "2026-11-10 10:06:00", true},
		{"0 0 1 jan,jul *", "2026-07-01 00:00:00", true},
		// 6 and 7 fields are passed through.
		{"30 0 12 * * *", "2026-11-10 12:00:00", true},
		{"0 0 12 * * * 2027", "2026-11-10 12:00:00", false},
		{"@daily", "2026-11-10 00:00:00", true},
		{"@hourly", "2026-11-10 13:00:00", true},
		{"@weekly", "2026-11-08 00:00:00", true},
	} {
		schedule, err := parseSchedulerCron(tc.expr)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		if _, got := schedule.matchesMinute(utc(tc.at)); got != tc.match {
			t.Errorf("%q at %s: got %v, want %v", tc.expr, tc.at, got, tc.match)
		}
	}
}

func TestCronRejectsInvalid(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "61 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *",
		"* * * * 0", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "MON * * * *", "? * * * *", "* * * * MON/2", "0 0 0 * * * 1969"} {
		if _, err := parseSchedulerCron(expr); err == nil {
			t.Errorf("%q must be rejected", expr)
		}
	}
}

func TestFireTimes(t *testing.T) {
	schedule, _ := parseSchedulerCron("*/15 * * * *")
	fires := schedule.fireTimes(utc("2026-11-10 10:00:00"), utc("2026-11-10 11:00:00"), 100)
	got := make([]string, len(fires))
	for i, fire := range fires {
		got[i] = fire.Format("15:04:05")
	}
	// (from, to]: 10:00 is excluded, 11:00 included.
	if strings.Join(got, ",") != "10:15:00,10:30:00,10:45:00,11:00:00" {
		t.Fatalf("fire times: %v", got)
	}
	everySecond, _ := parseSchedulerCron("*/10 * * * * *")
	if n := len(everySecond.fireTimes(utc("2026-11-10 10:00:30"), utc("2026-11-10 10:03:00"), 100)); n != 3 {
		t.Fatalf("several fire times within a minute must count once per minute, got %d", n)
	}
	if n := len(everySecond.fireTimes(utc("2026-11-10 10:00:00"), utc("2026-11-10 12:00:00"), 5)); n != 5 {
		t.Fatalf("max must be respected, got %d", n)
	}
}

func TestMissedFireTimes(t *testing.T) {
	grace := 30 * time.Minute
	fires := []time.Time{utc("2026-11-10 03:00:00"), utc("2026-11-10 04:00:00"), utc("2026-11-10 05:00:00"), utc("2026-11-10 06:00:00")}
	firstEnd, finished := utc("2026-11-10 03:05:00"), utc("2026-11-10 05:30:00")
	runs := []runSpan{
		{Started: utc("2026-11-10 02:59:30"), Finished: &firstEnd}, // 03:00 ran (scheduler starts slightly early)
		{Started: utc("2026-11-10 04:50:00"), Finished: &finished}, // still running at 05:00
	}
	missed := missedFireTimes(fires, runs, grace)
	var got []string
	for _, fire := range missed {
		got = append(got, fire.Format("15:04"))
	}
	// 04:00: no run; 05:00: previous run still running → not missed; 06:00: no run.
	if strings.Join(got, ",") != "04:00,06:00" {
		t.Fatalf("missed: %v", got)
	}

	late := []runSpan{{Started: utc("2026-11-10 04:31:00")}}
	if len(missedFireTimes(fires[1:2], late, grace)) != 1 {
		t.Fatal("a run starting after the grace period does not count")
	}
	unfinished := []runSpan{{Started: utc("2026-11-10 02:59:30")}}
	if len(missedFireTimes(fires, unfinished, grace)) != 0 {
		t.Fatal("while a run is unfinished the scheduler starts no new run (reported as hanging instead)")
	}
}

func TestMissedRunEventsAreSuppressedPerJob(t *testing.T) {
	plan := newPlanner(testArgs(t, ""), t0, nil)
	plan.handle(missedRunEvent(7, "Collector DE", "0 * * * *", utc("2026-11-10 03:00:00"), 30))
	plan.handle(missedRunEvent(7, "Collector DE", "0 * * * *", utc("2026-11-10 04:00:00"), 30))
	if len(plan.mails) != 1 || plan.suppressed != 1 {
		t.Fatalf("missed runs of one job share a fingerprint: mails=%d suppressed=%d", len(plan.mails), plan.suppressed)
	}
	if plan.mails[0].Subject != "[MitM][ERROR] Job Collector DE did not run at 2026-11-10 03:00:00 UTC" {
		t.Fatalf("subject: %s", plan.mails[0].Subject)
	}
	// A successful run of the job recovers the missed-run alert.
	plan.recover("Collector DE", "program_runs/9", 9, t0)
	if plan.recovered != 1 {
		t.Fatal("missed-run alert must be recovered by the next successful run")
	}
}

func TestAdminEventAllowlistsDetails(t *testing.T) {
	row := adminRow{ID: 42, Username: "Q225265", Action: "DELETE_CREDENTIAL",
		Details: `{"source_name":"ORA_HR","config_payload":"secret","count":3}`, TS: t0}
	event := adminEvent(row, []string{"source_name", "count", "missing"})
	text := fmt.Sprint(event.Fields)
	if !strings.Contains(text, "Detail source_name ORA_HR") || !strings.Contains(text, "Detail count 3") || strings.Contains(text, "secret") {
		t.Fatalf("details must be allowlisted: %v", event.Fields)
	}
	if event.Category != categorySecurity || event.Severity != severitySecurity || event.Subject != "DELETE_CREDENTIAL by Q225265" {
		t.Fatalf("unexpected event: %+v", event)
	}

	plan := newPlanner(testArgs(t, `,"sources":{"admin_audit_logs":{"enabled":true}}`), t0, nil)
	plan.handle(event)
	if plan.mails[0].Subject != "[MitM][SECURITY] DELETE_CREDENTIAL by Q225265" || plan.mails[0].Severity != severitySecurity {
		t.Fatalf("security mail: %+v", plan.mails[0])
	}
	// Same action by the same user is suppressed; another user is a new alert.
	plan.handle(adminEvent(adminRow{ID: 43, Username: "q225265", Action: "delete_credential", Details: "null", TS: t0}, nil))
	plan.handle(adminEvent(adminRow{ID: 44, Username: "Q999999", Action: "DELETE_CREDENTIAL", Details: "null", TS: t0}, nil))
	if len(plan.mails) != 2 || plan.suppressed != 1 {
		t.Fatalf("mails=%d suppressed=%d", len(plan.mails), plan.suppressed)
	}
}

func TestDefaultAdminActions(t *testing.T) {
	for _, required := range []string{"USER_LOGIN_FAILED", "UPSERT_CREDENTIAL", "UPSERT_TARGET_CREDENTIAL", "ASSIGN_ROLES", "KEY_ROTATION_SUCCESS"} {
		found := false
		for _, action := range defaultAdminActions {
			found = found || action == required
		}
		if !found {
			t.Errorf("default actions miss %s", required)
		}
	}
	for _, action := range defaultAdminActions {
		if strings.HasPrefix(action, "CLIENT_") || action != strings.ToUpper(action) {
			t.Errorf("default action %q must be an upper-case server action", action)
		}
	}
}

func TestPhase2Config(t *testing.T) {
	args := testArgs(t, "")
	if !enabled(args.Sources.MissedRuns.Enabled) || args.Sources.MissedRuns.GraceMinutes != 30 ||
		args.Sources.AdminAuditLogs.Enabled || len(args.Sources.AdminAuditLogs.Actions) != len(defaultAdminActions) ||
		args.Digest.Enabled || args.Heartbeat.Enabled || *args.Digest.HourUTC != 6 {
		t.Fatalf("unexpected phase 2 defaults: %+v", args)
	}
	dir := t.TempDir()
	for name, extra := range map[string]string{
		"digest without to":      `,"digest":{"enabled":true}`,
		"heartbeat bad hour":     `,"heartbeat":{"enabled":true,"hour_utc":24,"to":["a@example.com"]}`,
		"security without route": `,"sources":{"admin_audit_logs":{"enabled":true}}`,
		"empty actions":          `,"sources":{"admin_audit_logs":{"enabled":true,"actions":[]}}`,
	} {
		raw := fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"job","to":["a@example.com"]},
			{"category":"system","to":["a@example.com"]},{"category":"dlq","to":["a@example.com"]}]%s}`, dir, extra)
		if _, err := parseArgs(raw); err == nil {
			t.Errorf("%s: parseArgs accepted invalid arguments", name)
		}
	}
}

func TestDueSlot(t *testing.T) {
	before := utc("2026-11-10 05:59:00")
	if _, due := dueSlot(before, 6, nil); due {
		t.Fatal("not due before the hour")
	}
	slot, due := dueSlot(utc("2026-11-10 06:03:00"), 6, nil)
	if !due || !slot.Equal(utc("2026-11-10 06:00:00")) {
		t.Fatalf("due after the hour: %v %v", slot, due)
	}
	if _, due := dueSlot(utc("2026-11-10 18:00:00"), 6, &slot); due {
		t.Fatal("only once per day")
	}
	if _, due := dueSlot(utc("2026-11-11 06:00:00"), 6, &slot); !due {
		t.Fatal("due again the next day")
	}
}

func TestDigestRendering(t *testing.T) {
	oldest := utc("2026-11-09 12:00:00")
	data := digestData{
		Jobs: []jobStat{{"Collector DE", 96, 2}, {"Transformation Employee", 24, 0}},
		JobWarnings: []string{
			"cdh: CDH_RE: WARNING: skipped request v2_rooms {\"level_code\":\"L1\"}: giving up after 4 attempts: HTTP 503",
			"cdh: CDH_RE: WARNING: skipped request v2_rooms {\"level_code\":\"L2\"}: giving up after 4 attempts: HTTP 503",
		},
		SystemWarnings: []string{"http-server: HTTP 404 Not Found - GET /dashboard"},
		DLQOpen:        3, DLQNew: 1, DLQOldest: &oldest,
		TransformationErrors:     []countedText{{"email / mail: invalid format", 5}},
		TransformationErrorTotal: 5,
		OpenAlerts:               []openAlert{{Category: "job", Summary: "Job Collector DE failed", Suppressed: 4}},
		OpenAlertCount:           1,
	}
	args := testArgs(t, `,"digest":{"enabled":true,"to":["ops@example.com"]}`)
	mail := renderDigest(data, args, utc("2026-11-10 06:00:00"))
	for _, want := range []string{
		"Collector DE", "96 runs, 2 failed",
		"Job warnings (WARNING:): 2", "   2× cdh: CDH_RE: WARNING: skipped request v2_rooms",
		"Dead letter queue: 3 unresolved, 1 new, oldest 2026-11-09 12:00:00 UTC",
		"Transformation errors: 5", "[job] Job Collector DE failed (+4 suppressed)",
	} {
		if !strings.Contains(mail.Body, want) {
			t.Errorf("digest body misses %q:\n%s", want, mail.Body)
		}
	}
	if mail.Subject != "[MitM][INFO] Daily digest 2026-11-10: 1 jobs with failures, 1 open alerts" || mail.To[0] != "ops@example.com" {
		t.Fatalf("digest mail: %+v", mail)
	}

	hidden := renderDigest(data, testArgs(t, `,"metadata_only":true,"digest":{"enabled":true,"to":["ops@example.com"]}`), utc("2026-11-10 06:00:00"))
	if strings.Contains(hidden.Body, "skipped request") || strings.Contains(hidden.Body, "invalid format") {
		t.Fatalf("metadata_only digest must not contain texts:\n%s", hidden.Body)
	}
}

func TestHeartbeatRendering(t *testing.T) {
	ts := utc("2026-11-10 06:00:00")
	data := heartbeatData{
		Cursors:    []*cursor{{Source: sourceSystemLogs, LastID: "14744"}, {Source: sourceMissedRuns, LastTS: &ts}},
		OpenAlerts: 2, Runs: 120, Failed: 3,
		InvalidCrons: []string{"Broken Job (* * *)"},
	}
	mail := renderHeartbeat(data, testArgs(t, `,"heartbeat":{"enabled":true,"to":["admin@example.com"]}`), ts)
	for _, want := range []string{"Job runs (24 h): 120, failed: 3", "Open alerts:     2", "Broken Job (* * *)", "system_logs", "14744"} {
		if !strings.Contains(mail.Body, want) {
			t.Errorf("heartbeat body misses %q:\n%s", want, mail.Body)
		}
	}
	if mail.Subject != "[MitM][INFO] Monitor heartbeat 2026-11-10: 2 open alerts" || mail.Name != mailName([]string{"heartbeat/2026-11-10"}) {
		t.Fatalf("heartbeat mail: %+v", mail)
	}
}
