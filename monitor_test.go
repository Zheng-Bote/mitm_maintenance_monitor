// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testArgs(t *testing.T, extra string) MonitorArgs {
	t.Helper()
	raw := fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["ops@example.com"]},
		{"category":"system","to":["admin@example.com"],"cc":["ops@example.com"]}]%s}`, t.TempDir(), extra)
	args, err := parseArgs(raw)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

var t0 = time.Date(2026, 11, 9, 16, 42, 59, 0, time.UTC)

func failedEvent(runID int64, errorLine string) Event {
	code := int32(1)
	return failedRunEvent(finishedRun{ID: runID, Job: "Collector DE", ExitCode: &code, Started: t0.Add(-time.Minute), Finished: t0},
		[]string{errorLine})
}

func TestParseArgsDefaults(t *testing.T) {
	args := testArgs(t, "")
	if *args.SuppressRepeatMinutes != 240 || !*args.RecoveryMail || args.MaxMailsPerRun != 20 ||
		args.MaxMessageChars != 500 || args.SubjectPrefix != "[MitM]" || args.Sources.ProgramRuns.HangingMinutes != 120 ||
		args.Sources.SystemLogs.MinLevel != "ERROR" || args.Sources.SystemLogs.ExcludeComponents[0] != componentName {
		t.Fatalf("unexpected defaults: %+v", args)
	}
	if levels := levelsFrom("WARN"); strings.Join(levels, ",") != "WARN,WARNING,ERROR" {
		t.Fatalf("levelsFrom(WARN) = %v", levels)
	}
}

func TestParseArgsRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	for name, raw := range map[string]string{
		"empty":           ``,
		"no mail_dir":     `{"routing":[{"category":"*","to":["a@example.com"]}]}`,
		"no routing":      fmt.Sprintf(`{"mail_dir":%q}`, dir),
		"missing route":   fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"job","to":["a@example.com"]}]}`, dir),
		"bad address":     fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["Ops <a@example.com>"]}]}`, dir),
		"header inject":   fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["a@example.com\nBcc: x@evil.com"]}]}`, dir),
		"unknown level":   fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["a@example.com"]}],"sources":{"system_logs":{"min_level":"FATAL"}}}`, dir),
		"negative window": fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["a@example.com"]}],"suppress_repeat_minutes":-1}`, dir),
	} {
		if _, err := parseArgs(raw); err == nil {
			t.Errorf("%s: parseArgs accepted invalid arguments", name)
		}
	}
	// A route is only required for enabled categories.
	if _, err := parseArgs(fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"job","to":["a@example.com"]}],
		"sources":{"system_logs":{"enabled":false},"dead_letter_queue":{"enabled":false}}}`, dir)); err != nil {
		t.Fatalf("parseArgs rejected valid arguments: %v", err)
	}
}

func TestAddressesMatchSendmail(t *testing.T) {
	for address, valid := range map[string]bool{
		"ops@example.com":                  true,
		"first.last@sub.example.de":        true,
		"ops@localhost":                    false, // sendmail requires a dot in the domain
		"ops@.example.com":                 false,
		"ops@example.com.":                 false,
		"@example.com":                     false,
		"ops@@example.com":                 false,
		"Ops <ops@example.com>":            false,
		"ops@example.com\nBcc: x@evil.com": false,
	} {
		if got := validateAddress(address) == nil; got != valid {
			t.Errorf("validateAddress(%q) valid=%v, want %v", address, got, valid)
		}
	}
	if _, err := parseArgs(fmt.Sprintf(`{"mail_dir":%q,"routing":[{"category":"*","to":["ops@localhost"]}]}`, t.TempDir())); err == nil {
		t.Fatal("parseArgs must reject recipients sendmail cannot deliver to")
	}
}

func TestRecipients(t *testing.T) {
	args := testArgs(t, "")
	if to, cc := args.recipients(categorySystem); strings.Join(to, ",") != "admin@example.com" || strings.Join(cc, ",") != "ops@example.com" {
		t.Fatalf("system recipients: %v / %v", to, cc)
	}
	if to, _ := args.recipients(categoryDLQ); strings.Join(to, ",") != "ops@example.com" {
		t.Fatalf("dlq must fall back to *: %v", to)
	}
}

func TestFingerprintIgnoresVariableParts(t *testing.T) {
	a := failedEvent(3732, "ERROR: query failed for run 3732 at 16:42 (id 3f2a9c1e-1111-2222-3333-444455556666)")
	b := failedEvent(3790, "ERROR: query failed for run 3790 at 17:12 (id 9b8a7c6d-aaaa-bbbb-cccc-ddddeeeeffff)")
	c := failedEvent(3791, "ERROR: connection refused")
	if a.fingerprint() != b.fingerprint() || a.fingerprint() == c.fingerprint() {
		t.Fatal("fingerprint must ignore numbers and UUIDs but distinguish different errors")
	}
}

func TestSuppressionAndRepeatMail(t *testing.T) {
	args := testArgs(t, "")
	alerts := map[string]*Alert{}

	first := newPlanner(args, t0, alerts)
	first.handle(failedEvent(1, "ERROR: boom"))
	first.handle(failedEvent(2, "ERROR: boom")) // same run of the monitor: suppressed
	if len(first.mails) != 1 || first.suppressed != 1 {
		t.Fatalf("first run: mails=%d suppressed=%d", len(first.mails), first.suppressed)
	}

	inWindow := newPlanner(args, t0.Add(30*time.Minute), alerts)
	inWindow.handle(failedEvent(3, "ERROR: boom"))
	if len(inWindow.mails) != 0 {
		t.Fatal("repeated alert within the window must not be mailed")
	}

	afterWindow := newPlanner(args, t0.Add(5*time.Hour), alerts)
	afterWindow.handle(failedEvent(4, "ERROR: boom"))
	if len(afterWindow.mails) != 1 || !strings.Contains(afterWindow.mails[0].Body, "Suppressed:  2 further occurrences") {
		t.Fatalf("repeat mail after the window must report suppressed occurrences: %+v", afterWindow.mails)
	}
	for _, alert := range alerts {
		if alert.SuppressedCount != 0 || !alert.IsOpen {
			t.Fatalf("alert after repeat mail: %+v", alert)
		}
	}
}

func TestRecovery(t *testing.T) {
	args := testArgs(t, "")
	alerts := map[string]*Alert{}
	plan := newPlanner(args, t0, alerts)
	plan.handle(failedEvent(1, "ERROR: boom"))
	plan.recover("Collector DE", "program_runs/2", 2, t0.Add(time.Minute))
	plan.recover("Collector DE", "program_runs/3", 3, t0.Add(2*time.Minute)) // nothing open anymore

	if len(plan.mails) != 2 || plan.recovered != 1 {
		t.Fatalf("expected failure + one recovery mail, got %d mails", len(plan.mails))
	}
	recovery := plan.mails[1]
	if recovery.Severity != severityInfo || recovery.Subject != "[MitM][INFO] Job Collector DE recovered (RunID 2)" {
		t.Fatalf("unexpected recovery mail: %+v", recovery)
	}
	for _, alert := range alerts {
		if alert.IsOpen {
			t.Fatal("recovery must close the alert")
		}
	}

	// A new failure after the recovery opens a new alert and is mailed immediately.
	again := newPlanner(args, t0.Add(10*time.Minute), alerts)
	again.handle(failedEvent(4, "ERROR: boom"))
	if len(again.mails) != 1 {
		t.Fatal("failure after recovery must be mailed")
	}
}

func TestRecoveryMailDisabled(t *testing.T) {
	args := testArgs(t, `,"recovery_mail":false`)
	alerts := map[string]*Alert{}
	plan := newPlanner(args, t0, alerts)
	plan.handle(failedEvent(1, "ERROR: boom"))
	plan.recover("Collector DE", "program_runs/2", 2, t0)
	if len(plan.mails) != 1 || len(plan.dirtyAlerts()) != 1 || plan.dirtyAlerts()[0].IsOpen {
		t.Fatal("without recovery_mail the alert is closed silently")
	}
}

func TestLimitBundlesRest(t *testing.T) {
	args := testArgs(t, `,"max_mails_per_run":2`)
	plan := newPlanner(args, t0, nil)
	for i := 1; i <= 5; i++ {
		plan.handle(failedEvent(int64(i), fmt.Sprintf("ERROR: different problem %c", 'a'+i)))
	}
	mails, bundled := plan.limit()
	if len(mails) != 3 || bundled != 3 {
		t.Fatalf("expected 2 mails + 1 summary, got %d (bundled %d)", len(mails), bundled)
	}
	summary := mails[2]
	if summary.Subject != "[MitM][ERROR] 3 further alerts" || strings.Count(summary.Body, "\n- ") != 3 ||
		len(summary.Sources) != 3 || summary.To[0] != "ops@example.com" {
		t.Fatalf("unexpected summary mail: %+v", summary)
	}
}

func TestMetadataOnlyAndTruncation(t *testing.T) {
	long := "ERROR: " + strings.Repeat("x", 600)
	plan := newPlanner(testArgs(t, `,"max_message_chars":100,"frontend_url":"https://mitm.example.com"`), t0, nil)
	plan.handle(failedEvent(1, long))
	body := plan.mails[0].Body
	if strings.Contains(body, strings.Repeat("x", 100)) || !strings.Contains(body, "…") || !strings.Contains(body, "Details: https://mitm.example.com") {
		t.Fatalf("message must be truncated and contain the frontend link:\n%s", body)
	}

	hidden := newPlanner(testArgs(t, `,"metadata_only":true`), t0, nil)
	hidden.handle(failedEvent(1, "ERROR: secret customer value"))
	if strings.Contains(hidden.mails[0].Body, "secret customer value") || !strings.Contains(hidden.mails[0].Body, "metadata_only") {
		t.Fatalf("metadata_only must omit error texts:\n%s", hidden.mails[0].Body)
	}
}

func TestSystemLogFilter(t *testing.T) {
	for message, covered := range map[string]bool{
		"Job Collector DE failed with code 1 (RunID: 3732)": true,
		"Job X failed with code -1 (RunID: 7)":              true,
		"Error waiting for job X: no child process":         true,
		"Failed to spawn mitm-collector-ora: not found":     false,
	} {
		if coveredByProgramRuns.MatchString(message) != covered {
			t.Errorf("coveredByProgramRuns(%q) != %v", message, covered)
		}
	}
}

func TestRenderAndWriteMail(t *testing.T) {
	dir := t.TempDir()
	plan := newPlanner(testArgs(t, ""), t0, nil)
	plan.handle(failedEvent(3732, "Employee: mirror-dev_employee: ERROR: ORA-00904"))
	mail := plan.mails[0]
	mail.Subject += "\r\nBcc: attacker@example.com"

	if err := writeMail(dir, mail); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, mail.Name+mailFileSuffix)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mail file missing or wrong mode: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, mail.Name+tempFileSuffix)); !os.IsNotExist(err) {
		t.Fatal("temporary file must be renamed")
	}
	content, _ := os.ReadFile(path)
	headers, body, found := strings.Cut(string(content), "\n\n")
	if !found {
		t.Fatalf("missing empty line between header block and body:\n%s", content)
	}
	for _, want := range []string{
		"To: ops@example.com",
		"Subject: [MitM][ERROR] Job Collector DE failed (RunID 3732, exit code 1) Bcc: attacker@example.com",
		"X-MitM-Severity: ERROR",
		"X-MitM-Category: job",
		"X-MitM-Source: program_runs/3732",
	} {
		if !strings.Contains(headers, want) {
			t.Errorf("header %q missing in:\n%s", want, headers)
		}
	}
	if strings.Count(headers, "\n")+1 != 5 {
		t.Errorf("unexpected header lines (header injection?):\n%s", headers)
	}
	if !strings.Contains(body, "- Employee: mirror-dev_employee: ERROR: ORA-00904") || !strings.Contains(body, "Exit code:   1") {
		t.Errorf("unexpected body:\n%s", body)
	}

	// Same sources → same file name (a repeated run overwrites instead of duplicating).
	if mailName([]string{"program_runs/3732"}) != mail.Name {
		t.Fatal("mail names must be deterministic")
	}
}

func TestEnsureMailDir(t *testing.T) {
	base := t.TempDir()

	// Missing directory (with missing parent) is created with mode 0700.
	missing := filepath.Join(base, "a", "outbox")
	notes, err := ensureMailDir(missing)
	if err != nil || len(notes) != 1 || !strings.HasPrefix(notes[0], "Created mail_dir") {
		t.Fatalf("create: notes=%v err=%v", notes, err)
	}
	if info, _ := os.Stat(missing); info == nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created directory: %v", info)
	}

	// Correct mode: nothing to report, no probe file left.
	notes, err = ensureMailDir(missing)
	if err != nil || len(notes) != 0 {
		t.Fatalf("existing 0700: notes=%v err=%v", notes, err)
	}
	if entries, _ := os.ReadDir(missing); len(entries) != 0 {
		t.Fatalf("write check must not leave files: %v", entries)
	}

	// Too open (0755) and too restrictive (0500) modes are set to 0700.
	for _, mode := range []os.FileMode{0o755, 0o500} {
		os.Chmod(missing, mode)
		notes, err = ensureMailDir(missing)
		info, _ := os.Stat(missing)
		if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "mode set to 0700") || info.Mode().Perm() != 0o700 {
			t.Fatalf("mode %04o: notes=%v err=%v mode=%04o", mode, notes, err, info.Mode().Perm())
		}
	}

	// A file is not a directory.
	file := filepath.Join(base, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := ensureMailDir(file); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file: %v", err)
	}

	// A path below a file is not accessible.
	if _, err := ensureMailDir(filepath.Join(file, "outbox")); err == nil || !strings.Contains(err.Error(), "is not accessible") {
		t.Fatalf("below file: %v", err)
	}
}

func TestDescribePathShowsWorkingDirectory(t *testing.T) {
	cwd, _ := os.Getwd()
	text := describePath("../smtp_out")
	if !strings.Contains(text, `"../smtp_out"`) || !strings.Contains(text, filepath.Join(filepath.Dir(cwd), "smtp_out")) ||
		!strings.Contains(text, "working directory "+cwd) {
		t.Fatalf("describePath: %s", text)
	}
	if describePath("/var/lib/mitm/outbox") != `"/var/lib/mitm/outbox"` {
		t.Fatal("absolute paths are only quoted")
	}
}
