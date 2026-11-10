/**
 * SPDX-FileComment: MitM Maintenance Monitor - daily mails
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file daily.go
 * @brief Daily digest (warnings and backlog of the last 24 hours) and heartbeat mail.
 * @version 0.2.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sourceDigest    = "digest"
	sourceHeartbeat = "heartbeat"
	maxDigestGroups = 20
)

// dueSlot returns today's slot (hour UTC) and whether the daily mail is due: the slot has passed
// and no mail was written for it yet.
func dueSlot(now time.Time, hour int, last *time.Time) (time.Time, bool) {
	now = now.UTC()
	slot := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if now.Before(slot) || (last != nil && !last.Before(slot)) {
		return slot, false
	}
	return slot, true
}

// countedText is a group of similar texts (numbers and UUIDs ignored) with its first example.
type countedText struct {
	Example string
	Count   int
}

func groupTexts(texts []string, max int) (groups []countedText, total int) {
	index := map[string]int{}
	for _, text := range texts {
		key := normalizeError(text)
		if i, ok := index[key]; ok {
			groups[i].Count++
			continue
		}
		index[key] = len(groups)
		groups = append(groups, countedText{Example: text, Count: 1})
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Count > groups[j].Count })
	if len(groups) > max {
		groups = groups[:max]
	}
	return groups, len(texts)
}

type jobStat struct {
	Job          string
	Runs, Failed int
}

type openAlert struct {
	Category, Summary string
	LastMailed        *time.Time
	Suppressed        int
}

// digestData is the content of the daily digest (last 24 hours).
type digestData struct {
	Jobs                        []jobStat
	JobWarnings, SystemWarnings []string
	DLQOpen, DLQNew             int
	DLQOldest                   *time.Time
	TransformationErrors        []countedText
	TransformationErrorTotal    int
	OpenAlerts                  []openAlert
	OpenAlertCount              int
}

func collectDigest(ctx context.Context, db *pgxpool.Pool) (digestData, error) {
	var data digestData
	rows, err := db.Query(ctx, `
		SELECT COALESCE(p.name, 'program ' || r.program_id::text, $1), count(*)::int,
		       (count(*) FILTER (WHERE NOT COALESCE(r.success, false)))::int
		FROM program_runs r LEFT JOIN scheduled_programs p ON p.id = r.program_id
		WHERE r.finished_at > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 1
	`, immediateJob)
	if err != nil {
		return data, fmt.Errorf("Failed to query run statistics: %w", err)
	}
	if data.Jobs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[jobStat]); err != nil {
		return data, fmt.Errorf("Failed to read run statistics: %w", err)
	}

	if data.JobWarnings, err = queryStrings(ctx, db, `
		SELECT message FROM job_audit_logs
		WHERE ts > now() - interval '24 hours' AND message LIKE '%WARNING:%'
		ORDER BY id LIMIT 5000`); err != nil {
		return data, err
	}
	if data.SystemWarnings, err = queryStrings(ctx, db, `
		SELECT component || ': ' || message FROM system_logs
		WHERE ts > now() - interval '24 hours' AND upper(level) IN ('WARN', 'WARNING')
		ORDER BY id LIMIT 5000`); err != nil {
		return data, err
	}

	if err := db.QueryRow(ctx, `
		SELECT (count(*) FILTER (WHERE NOT resolved))::int,
		       (count(*) FILTER (WHERE NOT resolved AND failed_at > now() - interval '24 hours'))::int,
		       min(failed_at) FILTER (WHERE NOT resolved)
		FROM dead_letter_queue
	`).Scan(&data.DLQOpen, &data.DLQNew, &data.DLQOldest); err != nil {
		return data, fmt.Errorf("Failed to query dead_letter_queue: %w", err)
	}

	// transformation_errors.created_at is TIMESTAMP (without time zone), written with NOW().
	rows, err = db.Query(ctx, `
		SELECT rule_name || ' / ' || failed_field || ': ' || error_message, count(*)::int
		FROM transformation_errors
		WHERE created_at > now()::timestamp - interval '24 hours'
		GROUP BY rule_name, failed_field, error_message ORDER BY 2 DESC
	`)
	if err != nil {
		return data, fmt.Errorf("Failed to query transformation_errors: %w", err)
	}
	errorsByRule, err := pgx.CollectRows(rows, pgx.RowToStructByPos[countedText])
	if err != nil {
		return data, fmt.Errorf("Failed to read transformation_errors: %w", err)
	}
	for _, group := range errorsByRule {
		data.TransformationErrorTotal += group.Count
	}
	if len(errorsByRule) > maxDigestGroups {
		errorsByRule = errorsByRule[:maxDigestGroups]
	}
	data.TransformationErrors = errorsByRule

	if data.OpenAlerts, data.OpenAlertCount, err = queryOpenAlerts(ctx, db); err != nil {
		return data, err
	}
	return data, nil
}

func queryStrings(ctx context.Context, db *pgxpool.Pool, sql string) ([]string, error) {
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("Failed to query digest data: %w", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("Failed to read digest data: %w", err)
	}
	return values, nil
}

func queryOpenAlerts(ctx context.Context, db *pgxpool.Pool) ([]openAlert, int, error) {
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*)::int FROM monitor_alerts WHERE is_open AND last_mailed_at IS NOT NULL").Scan(&count); err != nil {
		return nil, 0, fmt.Errorf("Failed to count open alerts: %w", err)
	}
	rows, err := db.Query(ctx, `
		SELECT category, summary, last_mailed_at, suppressed_count FROM monitor_alerts
		WHERE is_open AND last_mailed_at IS NOT NULL ORDER BY last_seen DESC LIMIT $1
	`, maxDigestGroups)
	if err != nil {
		return nil, 0, fmt.Errorf("Failed to query open alerts: %w", err)
	}
	alerts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[openAlert])
	if err != nil {
		return nil, 0, fmt.Errorf("Failed to read open alerts: %w", err)
	}
	return alerts, count, nil
}

func renderDigest(data digestData, args MonitorArgs, slot time.Time) Mail {
	var body strings.Builder
	fmt.Fprintf(&body, "Daily digest for the 24 hours before %s.\n", slot.Format(timeLayout))

	failedJobs := 0
	body.WriteString("\nJob runs\n")
	if len(data.Jobs) == 0 {
		body.WriteString("  (no finished runs)\n")
	}
	for _, job := range data.Jobs {
		fmt.Fprintf(&body, "  %-40s %4d runs, %d failed\n", job.Job, job.Runs, job.Failed)
		if job.Failed > 0 {
			failedJobs++
		}
	}

	writeGroups := func(title string, texts []string) {
		groups, total := groupTexts(texts, maxDigestGroups)
		fmt.Fprintf(&body, "\n%s: %d\n", title, total)
		if args.MetadataOnly {
			return
		}
		for _, group := range groups {
			fmt.Fprintf(&body, "  %4d× %s\n", group.Count, truncate(group.Example, args.MaxMessageChars))
		}
	}
	writeGroups("Job warnings (WARNING:)", data.JobWarnings)
	writeGroups("System warnings (WARN)", data.SystemWarnings)

	fmt.Fprintf(&body, "\nDead letter queue: %d unresolved, %d new", data.DLQOpen, data.DLQNew)
	if data.DLQOldest != nil {
		fmt.Fprintf(&body, ", oldest %s", data.DLQOldest.UTC().Format(timeLayout))
	}
	body.WriteString("\n")

	fmt.Fprintf(&body, "\nTransformation errors: %d\n", data.TransformationErrorTotal)
	if !args.MetadataOnly {
		for _, group := range data.TransformationErrors {
			fmt.Fprintf(&body, "  %4d× %s\n", group.Count, truncate(group.Example, args.MaxMessageChars))
		}
	}

	fmt.Fprintf(&body, "\nOpen alerts: %d\n", data.OpenAlertCount)
	for _, alert := range data.OpenAlerts {
		line := fmt.Sprintf("  [%s] %s", alert.Category, alert.Summary)
		if alert.Suppressed > 0 {
			line += fmt.Sprintf(" (+%d suppressed)", alert.Suppressed)
		}
		body.WriteString(line + "\n")
	}
	if args.FrontendURL != "" {
		body.WriteString("\nDetails: " + args.FrontendURL + "\n")
	}

	subject := fmt.Sprintf("%s[%s] Daily digest %s: %d jobs with failures, %d open alerts",
		args.SubjectPrefix, severityInfo, slot.Format("2006-01-02"), failedJobs, data.OpenAlertCount)
	return Mail{
		Name:     mailName([]string{"digest/" + slot.Format("2006-01-02")}),
		To:       args.Digest.To,
		Cc:       args.Digest.Cc,
		Subject:  subject,
		Severity: severityInfo,
		Category: sourceDigest,
		Sources:  []string{"digest/" + slot.Format("2006-01-02")},
		Body:     body.String(),
	}
}

// heartbeatData is the content of the heartbeat mail.
type heartbeatData struct {
	Cursors      []*cursor
	OpenAlerts   int
	Runs, Failed int
	InvalidCrons []string
}

func collectHeartbeat(ctx context.Context, db *pgxpool.Pool) (heartbeatData, error) {
	var data heartbeatData
	if err := db.QueryRow(ctx, `
		SELECT count(*)::int, (count(*) FILTER (WHERE NOT COALESCE(success, false)))::int
		FROM program_runs WHERE finished_at > now() - interval '24 hours'
	`).Scan(&data.Runs, &data.Failed); err != nil {
		return data, fmt.Errorf("Failed to query run statistics: %w", err)
	}
	if err := db.QueryRow(ctx, "SELECT count(*)::int FROM monitor_alerts WHERE is_open AND last_mailed_at IS NOT NULL").Scan(&data.OpenAlerts); err != nil {
		return data, fmt.Errorf("Failed to count open alerts: %w", err)
	}
	return data, nil
}

func renderHeartbeat(data heartbeatData, args MonitorArgs, slot time.Time) Mail {
	var body strings.Builder
	fmt.Fprintf(&body, "%s %s is running (heartbeat of %s).\n", appName, version, slot.Format("2006-01-02"))
	body.WriteString("If this daily mail does not arrive, the scheduler, the monitor or the mail delivery is not running.\n\n")
	fmt.Fprintf(&body, "Job runs (24 h): %d, failed: %d\n", data.Runs, data.Failed)
	fmt.Fprintf(&body, "Open alerts:     %d\n", data.OpenAlerts)
	if len(data.InvalidCrons) > 0 {
		body.WriteString("\nPrograms with an invalid cron expression (never started by the scheduler):\n")
		for _, program := range data.InvalidCrons {
			body.WriteString("  - " + program + "\n")
		}
	}
	body.WriteString("\nMonitored sources (position):\n")
	for _, c := range data.Cursors {
		position := c.LastID
		if c.LastTS != nil {
			position = strings.TrimSpace(c.LastTS.UTC().Format(timeLayout) + " " + c.LastID)
		}
		fmt.Fprintf(&body, "  %-18s %s\n", c.Source, position)
	}
	if args.FrontendURL != "" {
		body.WriteString("\nDetails: " + args.FrontendURL + "\n")
	}
	return Mail{
		Name:     mailName([]string{"heartbeat/" + slot.Format("2006-01-02")}),
		To:       args.Heartbeat.To,
		Cc:       args.Heartbeat.Cc,
		Subject:  fmt.Sprintf("%s[%s] Monitor heartbeat %s: %d open alerts", args.SubjectPrefix, severityInfo, slot.Format("2006-01-02"), data.OpenAlerts),
		Severity: severityInfo,
		Category: sourceHeartbeat,
		Sources:  []string{"heartbeat/" + slot.Format("2006-01-02")},
		Body:     body.String(),
	}
}

type dailyResult struct {
	mails             []Mail
	digest, heartbeat bool
}

// dailyMails writes the digest and the heartbeat once per day after their hour (UTC).
func dailyMails(ctx context.Context, db *pgxpool.Pool, args MonitorArgs, now time.Time, cursors map[string]*cursor, invalidCrons []string) (dailyResult, error) {
	var result dailyResult
	if args.Digest.Enabled {
		c := cursors[sourceDigest]
		if slot, due := dueSlot(now, *args.Digest.HourUTC, c.LastTS); due {
			data, err := collectDigest(ctx, db)
			if err != nil {
				return result, err
			}
			result.mails = append(result.mails, renderDigest(data, args, slot))
			c.LastTS, c.dirty, result.digest = &slot, true, true
		}
	}
	if args.Heartbeat.Enabled {
		c := cursors[sourceHeartbeat]
		if slot, due := dueSlot(now, *args.Heartbeat.HourUTC, c.LastTS); due {
			data, err := collectHeartbeat(ctx, db)
			if err != nil {
				return result, err
			}
			data.InvalidCrons = invalidCrons
			for _, source := range eventSources {
				data.Cursors = append(data.Cursors, cursors[source])
			}
			result.mails = append(result.mails, renderHeartbeat(data, args, slot))
			c.LastTS, c.dirty, result.heartbeat = &slot, true, true
		}
	}
	return result, nil
}
