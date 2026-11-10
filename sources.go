/**
 * SPDX-FileComment: MitM Maintenance Monitor - monitored sources
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file sources.go
 * @brief Database queries of program_runs, system_logs and dead_letter_queue, source cursors
 *        (monitor_state) and alert state (monitor_alerts).
 * @version 0.1.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sourceProgramRuns = "program_runs"
	sourceSystemLogs  = "system_logs"
	sourceDLQ         = "dead_letter_queue"

	// settleDelay: only entries older than this are processed, so rows committed late are not skipped.
	settleDelay  = 60 * time.Second
	batchLimit   = 1000
	maxRunErrors = 20
)

// coveredByProgramRuns matches scheduler messages that are already reported via program_runs.
var coveredByProgramRuns = regexp.MustCompile(`^(Job .+ failed with code -?\d+ \(RunID: \d+\)|Error waiting for job .+)$`)

// cursor is the last processed position of a source (table monitor_state).
type cursor struct {
	Source string
	LastTS *time.Time
	LastID string
	isNew  bool // no row yet: initialize without mails
	dirty  bool
}

// finishedRun is a program_runs row with finished_at set.
type finishedRun struct {
	ID       int64
	Job      string
	ExitCode *int32
	Success  bool
	Started  time.Time
	Finished time.Time
}

func dbNow(ctx context.Context, db *pgxpool.Pool) (time.Time, error) {
	var now time.Time
	if err := db.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return now, fmt.Errorf("Failed to read database time: %w", err)
	}
	return now, nil
}

func loadCursors(ctx context.Context, db *pgxpool.Pool) (map[string]*cursor, error) {
	rows, err := db.Query(ctx, "SELECT source, last_ts, last_id FROM monitor_state")
	if err != nil {
		return nil, fmt.Errorf("Failed to load monitor_state (migration 007 applied?): %w", err)
	}
	defer rows.Close()
	cursors := map[string]*cursor{}
	for rows.Next() {
		c := &cursor{}
		if err := rows.Scan(&c.Source, &c.LastTS, &c.LastID); err != nil {
			return nil, fmt.Errorf("Failed to read monitor_state: %w", err)
		}
		cursors[c.Source] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Failed to read monitor_state: %w", err)
	}
	for _, source := range []string{sourceProgramRuns, sourceSystemLogs, sourceDLQ} {
		if cursors[source] == nil {
			cursors[source] = &cursor{Source: source, isNew: true}
		}
	}
	return cursors, nil
}

// --- program_runs ----------------------------------------------------------------

// collectFinishedRuns returns settled finished runs after the cursor, ordered by finish time.
func collectFinishedRuns(ctx context.Context, db *pgxpool.Pool, c *cursor, settled time.Time) ([]finishedRun, error) {
	if c.isNew {
		c.LastTS, c.LastID, c.dirty = &settled, "0", true
		return nil, nil
	}
	lastID, _ := strconv.ParseInt(c.LastID, 10, 64)
	rows, err := db.Query(ctx, `
		SELECT r.id, COALESCE(p.name, 'program ' || r.program_id::text), r.exit_code,
		       COALESCE(r.success, false), r.started_at, r.finished_at
		FROM program_runs r
		LEFT JOIN scheduled_programs p ON p.id = r.program_id
		WHERE r.finished_at IS NOT NULL AND r.finished_at <= $1
		  AND (r.finished_at, r.id) > ($2::timestamptz, $3::int)
		ORDER BY r.finished_at, r.id
		LIMIT $4
	`, settled, c.LastTS, lastID, batchLimit)
	if err != nil {
		return nil, fmt.Errorf("Failed to query program_runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (finishedRun, error) {
		var run finishedRun
		err := row.Scan(&run.ID, &run.Job, &run.ExitCode, &run.Success, &run.Started, &run.Finished)
		return run, err
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to read program_runs: %w", err)
	}
	if len(runs) > 0 {
		last := runs[len(runs)-1]
		c.LastTS, c.LastID, c.dirty = &last.Finished, strconv.FormatInt(last.ID, 10), true
	}
	return runs, nil
}

// runErrors returns the ERROR lines of a run; fallback: its last "failed" status message.
func runErrors(ctx context.Context, db *pgxpool.Pool, runID int64) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT message FROM job_audit_logs
		WHERE run_id = $1 AND message LIKE '%ERROR:%'
		ORDER BY id LIMIT $2
	`, runID, maxRunErrors)
	if err != nil {
		return nil, fmt.Errorf("Failed to query job_audit_logs: %w", err)
	}
	messages, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("Failed to read job_audit_logs: %w", err)
	}
	if len(messages) > 0 {
		return messages, nil
	}
	var message string
	err = db.QueryRow(ctx, `
		SELECT COALESCE(message, '') FROM job_status_events
		WHERE run_id = $1 AND status = 'failed'
		ORDER BY id DESC LIMIT 1
	`, runID).Scan(&message)
	if errors.Is(err, pgx.ErrNoRows) || message == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to query job_status_events: %w", err)
	}
	return []string{message}, nil
}

func failedRunEvent(run finishedRun, errorLines []string) Event {
	exitCode := "unknown"
	if run.ExitCode != nil {
		exitCode = strconv.Itoa(int(*run.ExitCode))
	}
	errorKey := "exit code " + exitCode
	if len(errorLines) > 0 {
		errorKey = errorLines[0]
	}
	return Event{
		Source:   fmt.Sprintf("%s/%d", sourceProgramRuns, run.ID),
		Category: categoryJob,
		Group:    run.Job,
		JobName:  run.Job,
		Subject:  fmt.Sprintf("Job %s failed (RunID %d, exit code %s)", run.Job, run.ID, exitCode),
		Fields: [][2]string{
			{"Job", run.Job},
			{"Run ID", strconv.FormatInt(run.ID, 10)},
			{"Exit code", exitCode},
			{"Started", run.Started.UTC().Format(timeLayout)},
			{"Finished", run.Finished.UTC().Format(timeLayout)},
		},
		Errors:   errorLines,
		ErrorKey: errorKey,
		Time:     run.Finished,
	}
}

// collectHangingRuns returns one event per run without finished_at that runs longer than hangingMinutes.
func collectHangingRuns(ctx context.Context, db *pgxpool.Pool, now time.Time, hangingMinutes int) ([]Event, error) {
	rows, err := db.Query(ctx, `
		SELECT r.id, COALESCE(p.name, 'program ' || r.program_id::text), COALESCE(r.pid, 0), r.started_at
		FROM program_runs r
		LEFT JOIN scheduled_programs p ON p.id = r.program_id
		WHERE r.finished_at IS NULL AND r.started_at < $1
		ORDER BY r.id
		LIMIT $2
	`, now.Add(-time.Duration(hangingMinutes)*time.Minute), batchLimit)
	if err != nil {
		return nil, fmt.Errorf("Failed to query hanging program_runs: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var id int64
		var pid int32
		var job string
		var started time.Time
		if err := row.Scan(&id, &job, &pid, &started); err != nil {
			return Event{}, err
		}
		minutes := int(now.Sub(started).Minutes())
		source := fmt.Sprintf("%s/%d", sourceProgramRuns, id)
		return Event{
			Source:   source + "/hanging",
			Category: categoryJob,
			Group:    "hanging " + source,
			JobName:  job,
			Subject:  fmt.Sprintf("Job %s hanging (RunID %d, running %d min)", job, id, minutes),
			Fields: [][2]string{
				{"Job", job},
				{"Run ID", strconv.FormatInt(id, 10)},
				{"PID", strconv.Itoa(int(pid))},
				{"Started", started.UTC().Format(timeLayout)},
				{"Running", fmt.Sprintf("%d minutes (limit %d)", minutes, hangingMinutes)},
			},
			ErrorKey: "hanging",
			Time:     now,
		}, nil
	})
}

// --- system_logs -----------------------------------------------------------------

func collectSystemLogs(ctx context.Context, db *pgxpool.Pool, c *cursor, settled time.Time, cfg SystemLogsConfig) ([]Event, error) {
	var maxSettled int64
	if err := db.QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM system_logs WHERE ts <= $1", settled).Scan(&maxSettled); err != nil {
		return nil, fmt.Errorf("Failed to query system_logs: %w", err)
	}
	if c.isNew {
		c.LastID, c.dirty = strconv.FormatInt(maxSettled, 10), true
		return nil, nil
	}
	lastID, _ := strconv.ParseInt(c.LastID, 10, 64)
	rows, err := db.Query(ctx, `
		SELECT id, level, component, message, ts FROM system_logs
		WHERE id > $1 AND id <= $2 AND upper(level) = ANY($3)
		ORDER BY id
		LIMIT $4
	`, lastID, maxSettled, levelsFrom(cfg.MinLevel), batchLimit)
	if err != nil {
		return nil, fmt.Errorf("Failed to query system_logs: %w", err)
	}
	type logRow struct {
		ID                        int64
		Level, Component, Message string
		TS                        time.Time
	}
	logs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[logRow])
	if err != nil {
		return nil, fmt.Errorf("Failed to read system_logs: %w", err)
	}

	next := maxSettled
	if len(logs) == batchLimit {
		next = logs[len(logs)-1].ID
	}
	if next > lastID {
		c.LastID, c.dirty = strconv.FormatInt(next, 10), true
	}

	var events []Event
	for _, log := range logs {
		if slices.Contains(cfg.ExcludeComponents, log.Component) || coveredByProgramRuns.MatchString(log.Message) {
			continue
		}
		events = append(events, Event{
			Source:   fmt.Sprintf("%s/%d", sourceSystemLogs, log.ID),
			Category: categorySystem,
			Group:    log.Component,
			Subject:  fmt.Sprintf("%s: %s", log.Component, truncate(log.Message, 120)),
			Fields: [][2]string{
				{"Component", log.Component},
				{"Level", log.Level},
				{"Time", log.TS.UTC().Format(timeLayout)},
				{"Log ID", strconv.FormatInt(log.ID, 10)},
			},
			Errors:   []string{log.Message},
			ErrorKey: log.Message,
			Time:     log.TS,
		})
	}
	return events, nil
}

// --- dead_letter_queue -----------------------------------------------------------

func collectDeadLetters(ctx context.Context, db *pgxpool.Pool, c *cursor, settled time.Time) ([]Event, error) {
	if c.isNew {
		c.LastTS, c.LastID, c.dirty = &settled, "", true
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT id::text, COALESCE(package_id::text, ''), COALESCE(error_code, ''),
		       COALESCE(error_message, ''), failed_at
		FROM dead_letter_queue
		WHERE resolved = false AND failed_at <= $1
		  AND (failed_at, id::text) > ($2::timestamptz, $3::text)
		ORDER BY failed_at, id::text
		LIMIT $4
	`, settled, c.LastTS, c.LastID, batchLimit)
	if err != nil {
		return nil, fmt.Errorf("Failed to query dead_letter_queue: %w", err)
	}
	type dlqRow struct {
		ID, PackageID, Code, Message string
		FailedAt                     time.Time
	}
	letters, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dlqRow])
	if err != nil {
		return nil, fmt.Errorf("Failed to read dead_letter_queue: %w", err)
	}
	if len(letters) > 0 {
		last := letters[len(letters)-1]
		c.LastTS, c.LastID, c.dirty = &last.FailedAt, last.ID, true
	}

	events := make([]Event, 0, len(letters))
	for _, letter := range letters {
		code := letter.Code
		if code == "" {
			code = "UNKNOWN"
		}
		events = append(events, Event{
			Source:   sourceDLQ + "/" + letter.ID,
			Category: categoryDLQ,
			Group:    code,
			Subject:  "Dead letter: " + code,
			Fields: [][2]string{
				{"DLQ ID", letter.ID},
				{"Package ID", letter.PackageID},
				{"Error code", code},
				{"Failed at", letter.FailedAt.UTC().Format(timeLayout)},
			},
			Errors:   []string{letter.Message},
			ErrorKey: letter.Message,
			Time:     letter.FailedAt,
		})
	}
	return events, nil
}

// --- monitor_alerts / monitor_state ----------------------------------------------

// loadAlerts loads the alerts of the given fingerprints and all open job alerts (recovery).
func loadAlerts(ctx context.Context, db *pgxpool.Pool, fingerprints []string) (map[string]*Alert, error) {
	rows, err := db.Query(ctx, `
		SELECT fingerprint, category, COALESCE(job_name, ''), summary, first_seen, last_seen,
		       last_mailed_at, suppressed_count, is_open
		FROM monitor_alerts
		WHERE fingerprint = ANY($1) OR (is_open AND category = $2)
	`, fingerprints, categoryJob)
	if err != nil {
		return nil, fmt.Errorf("Failed to load monitor_alerts (migration 007 applied?): %w", err)
	}
	alerts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Alert, error) {
		alert := &Alert{}
		err := row.Scan(&alert.Fingerprint, &alert.Category, &alert.JobName, &alert.Summary, &alert.FirstSeen,
			&alert.LastSeen, &alert.LastMailedAt, &alert.SuppressedCount, &alert.IsOpen)
		return alert, err
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to read monitor_alerts: %w", err)
	}
	result := make(map[string]*Alert, len(alerts))
	for _, alert := range alerts {
		result[alert.Fingerprint] = alert
	}
	return result, nil
}

// commitState stores the changed cursors and alerts in one transaction.
func commitState(ctx context.Context, db *pgxpool.Pool, cursors map[string]*cursor, alerts []*Alert) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("Failed to begin state transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	batch := &pgx.Batch{}
	for _, c := range cursors {
		if !c.dirty {
			continue
		}
		batch.Queue(`
			INSERT INTO monitor_state (source, last_ts, last_id, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (source) DO UPDATE
			SET last_ts = EXCLUDED.last_ts, last_id = EXCLUDED.last_id, updated_at = NOW()
		`, c.Source, c.LastTS, c.LastID)
	}
	for _, alert := range alerts {
		var jobName *string
		if alert.JobName != "" {
			jobName = &alert.JobName
		}
		batch.Queue(`
			INSERT INTO monitor_alerts (fingerprint, category, job_name, summary, first_seen, last_seen,
			                            last_mailed_at, suppressed_count, is_open)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (fingerprint) DO UPDATE
			SET category = EXCLUDED.category, job_name = EXCLUDED.job_name, summary = EXCLUDED.summary,
			    first_seen = EXCLUDED.first_seen, last_seen = EXCLUDED.last_seen,
			    last_mailed_at = EXCLUDED.last_mailed_at, suppressed_count = EXCLUDED.suppressed_count,
			    is_open = EXCLUDED.is_open
		`, alert.Fingerprint, alert.Category, jobName, alert.Summary, alert.FirstSeen, alert.LastSeen,
			alert.LastMailedAt, alert.SuppressedCount, alert.IsOpen)
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("Failed to store monitor state: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("Failed to commit monitor state: %w", err)
	}
	return nil
}
