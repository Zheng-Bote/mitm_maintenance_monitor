/**
 * SPDX-FileComment: MitM Maintenance Monitor - phase 2 sources
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file sources_phase2.go
 * @brief Missed runs of scheduled programs (cron_expr) and critical admin_audit_logs actions.
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
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sourceMissedRuns = "missed_runs"
	sourceAdminAudit = "admin_audit_logs"

	// maxMissedWindow limits how far back missed runs are evaluated after a monitor outage.
	maxMissedWindow = 24 * time.Hour
	// maxFiresPerProgram limits the fire times evaluated per program and run.
	maxFiresPerProgram = 200
	// startTolerance: the scheduler starts a due job up to a minute before its exact fire time.
	startTolerance = time.Minute
)

// runSpan is the start and end of one program run.
type runSpan struct {
	Started  time.Time
	Finished *time.Time
}

// missedFireTimes returns the fire times without a run that started within [T - 1 min, T + grace].
// A fire time is not missed while a previous run was still running: the scheduler never starts
// a job twice in parallel (long runs are reported as hanging instead).
func missedFireTimes(fires []time.Time, runs []runSpan, grace time.Duration) []time.Time {
	var missed []time.Time
	for _, fire := range fires {
		ran := false
		for _, run := range runs {
			startedInWindow := !run.Started.Before(fire.Add(-startTolerance)) && !run.Started.After(fire.Add(grace))
			stillRunning := run.Started.Before(fire) && (run.Finished == nil || run.Finished.After(fire))
			if startedInWindow || stillRunning {
				ran = true
				break
			}
		}
		if !ran {
			missed = append(missed, fire)
		}
	}
	return missed
}

func missedRunEvent(programID int64, job, cronExpr string, fire time.Time, graceMinutes int) Event {
	return Event{
		Source:   fmt.Sprintf("scheduled_programs/%d/missed/%d", programID, fire.Unix()),
		Category: categoryJob,
		Group:    job,
		JobName:  job,
		Subject:  fmt.Sprintf("Job %s did not run at %s", job, fire.UTC().Format(timeLayout)),
		Fields: [][2]string{
			{"Job", job},
			{"Cron", cronExpr},
			{"Expected", fire.UTC().Format(timeLayout)},
			{"Grace", fmt.Sprintf("%d minutes", graceMinutes)},
		},
		Errors:   []string{"No run of this job started within the grace period after its scheduled time."},
		ErrorKey: "missed run",
		Time:     fire,
	}
}

// missedRunsResult carries the missed-run events and the programs with an invalid cron expression.
type missedRunsResult struct {
	events       []Event
	invalidCrons []string
	truncated    bool
}

// collectMissedRuns evaluates the fire times of all enabled programs in (cursor, now - grace].
func collectMissedRuns(ctx context.Context, db *pgxpool.Pool, c *cursor, now time.Time, cfg MissedRunsConfig) (missedRunsResult, error) {
	var result missedRunsResult
	grace := time.Duration(cfg.GraceMinutes) * time.Minute
	to := now.Add(-grace)
	if c.isNew || c.LastTS == nil {
		c.LastTS, c.LastID, c.dirty = &to, "", true
		return result, nil
	}
	from := *c.LastTS
	if !to.After(from) {
		return result, nil
	}
	if to.Sub(from) > maxMissedWindow {
		from = to.Add(-maxMissedWindow)
		result.truncated = true
	}

	rows, err := db.Query(ctx, `
		SELECT id, name, cron_expr, COALESCE(GREATEST(created_at, updated_at), 'epoch'::timestamptz)
		FROM scheduled_programs WHERE enabled ORDER BY id
	`)
	if err != nil {
		return result, fmt.Errorf("Failed to query scheduled_programs: %w", err)
	}
	type programRow struct {
		ID      int64
		Name    string
		Cron    string
		Changed time.Time
	}
	programs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[programRow])
	if err != nil {
		return result, fmt.Errorf("Failed to read scheduled_programs: %w", err)
	}

	for _, program := range programs {
		if slices.Contains(cfg.ExcludeJobs, program.Name) {
			continue
		}
		schedule, err := parseSchedulerCron(program.Cron)
		if err != nil {
			result.invalidCrons = append(result.invalidCrons, fmt.Sprintf("%s (%s)", program.Name, program.Cron))
			continue
		}
		start := from
		if program.Changed.After(start) { // created or changed later: its old schedule does not apply
			start = program.Changed
		}
		fires := schedule.fireTimes(start, to, maxFiresPerProgram)
		if len(fires) == 0 {
			continue
		}
		runRows, err := db.Query(ctx, `
			SELECT started_at, finished_at FROM program_runs
			WHERE program_id = $1 AND started_at <= $2 AND (finished_at IS NULL OR finished_at >= $3)
		`, program.ID, now, fires[0].Add(-startTolerance))
		if err != nil {
			return result, fmt.Errorf("Failed to query program_runs of %s: %w", program.Name, err)
		}
		runs, err := pgx.CollectRows(runRows, pgx.RowToStructByPos[runSpan])
		if err != nil {
			return result, fmt.Errorf("Failed to read program_runs of %s: %w", program.Name, err)
		}
		for _, fire := range missedFireTimes(fires, runs, grace) {
			result.events = append(result.events, missedRunEvent(program.ID, program.Name, program.Cron, fire, cfg.GraceMinutes))
		}
	}
	c.LastTS, c.dirty = &to, true
	return result, nil
}

// adminRow is one admin_audit_logs entry.
type adminRow struct {
	ID       int64
	Username string
	Action   string
	Details  string // JSON text
	TS       time.Time
}

func adminEvent(row adminRow, detailFields []string) Event {
	fields := [][2]string{
		{"Action", row.Action},
		{"User", row.Username},
		{"Time", row.TS.UTC().Format(timeLayout)},
		{"Log ID", strconv.FormatInt(row.ID, 10)},
	}
	// Only allowlisted detail fields; the details object is never copied completely.
	var details map[string]any
	if json.Unmarshal([]byte(row.Details), &details) == nil && details != nil {
		keys := make([]string, 0, len(detailFields))
		for _, key := range detailFields {
			if _, ok := details[key]; ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, ok := details[key].(string)
			if !ok {
				data, _ := json.Marshal(details[key])
				value = string(data)
			}
			fields = append(fields, [2]string{"Detail " + key, truncate(value, 200)})
		}
	}
	return Event{
		Source:   fmt.Sprintf("%s/%d", sourceAdminAudit, row.ID),
		Category: categorySecurity,
		Severity: severitySecurity,
		Group:    strings.ToUpper(row.Action) + "|" + strings.ToUpper(row.Username),
		Subject:  fmt.Sprintf("%s by %s", row.Action, row.Username),
		Fields:   fields,
		ErrorKey: strings.ToUpper(row.Action),
		Time:     row.TS,
	}
}

// collectAdminAudit returns new admin_audit_logs entries whose action is in the configured list
// (case-insensitive).
func collectAdminAudit(ctx context.Context, db *pgxpool.Pool, c *cursor, settled time.Time, cfg AdminAuditConfig) ([]Event, error) {
	var maxSettled int64
	if err := db.QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM admin_audit_logs WHERE ts <= $1", settled).Scan(&maxSettled); err != nil {
		return nil, fmt.Errorf("Failed to query admin_audit_logs: %w", err)
	}
	if c.isNew {
		c.LastID, c.dirty = strconv.FormatInt(maxSettled, 10), true
		return nil, nil
	}
	lastID, _ := strconv.ParseInt(c.LastID, 10, 64)
	actions := make([]string, len(cfg.Actions))
	for i, action := range cfg.Actions {
		actions[i] = strings.ToUpper(action)
	}
	rows, err := db.Query(ctx, `
		SELECT id, username, action, COALESCE(details::text, 'null'), ts FROM admin_audit_logs
		WHERE id > $1 AND id <= $2 AND upper(action) = ANY($3)
		ORDER BY id
		LIMIT $4
	`, lastID, maxSettled, actions, batchLimit)
	if err != nil {
		return nil, fmt.Errorf("Failed to query admin_audit_logs: %w", err)
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[adminRow])
	if err != nil {
		return nil, fmt.Errorf("Failed to read admin_audit_logs: %w", err)
	}

	next := maxSettled
	if len(entries) == batchLimit {
		next = entries[len(entries)-1].ID
	}
	if next > lastID {
		c.LastID, c.dirty = strconv.FormatInt(next, 10), true
	}
	events := make([]Event, 0, len(entries))
	for _, entry := range entries {
		events = append(events, adminEvent(entry, cfg.DetailFields))
	}
	return events, nil
}
