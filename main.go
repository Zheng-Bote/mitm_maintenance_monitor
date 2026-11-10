/**
 * SPDX-FileComment: MitM Maintenance Monitor
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file main.go
 * @brief Scheduled job that detects critical errors (failed and hanging job runs, system_logs errors,
 *        new dead letters) and writes them as *.SendMail files for mitm_maintenance_sendmail.
 * @version 0.2.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	appName = "MitM Maintenance Monitor"
	version = "0.2.0"
)

const componentName = "mitm_maintenance_monitor"

// TargetDBConfig defines the MitM database connection.
type TargetDBConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// StatusEvent is sent to the scheduler Unix socket.
type StatusEvent struct {
	RunID     int    `json:"run_id"`
	Type      string `json:"type"` // "status" or "audit"
	Component string `json:"component"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Progress  int    `json:"progress"`
}

// IPCClient sends events to the scheduler; a nil client is a no-op (standalone execution).
type IPCClient struct {
	SocketPath string
	RunID      int
	Component  string
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	cancel()
	if err != nil {
		log.Printf("%s failed: %v", appName, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// The monitor needs only the database configuration; the Master Key is not used.
	if dbCfg, err := fetchDBConfigFromScheduler(); err == nil {
		if dbCfg != "" {
			os.Setenv("MITM_DB_CONFIG_JSON", dbCfg)
		}
	} else if os.Getenv("RUN_ID") != "" && os.Getenv("SCHEDULER_SOCKET_PATH") != "" {
		log.Printf("[IPC Warning] Failed to get credentials from scheduler: %v", err)
	}

	version = strings.Split(version, "-")[0]
	ipc := newIPCClient()
	ipc.SendEvent("started", fmt.Sprintf("%s (%s) started", appName, version), 0)
	ipc.SendAudit(fmt.Sprintf("%s (%s) started", appName, version))

	rawArgs := ""
	if len(os.Args) >= 2 {
		rawArgs = os.Args[1]
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return fail(ipc, err)
	}

	targetCfg, configSource, err := loadTargetDBConfig()
	if err != nil {
		return fail(ipc, err)
	}
	ipc.SendAudit("Loaded database configuration from " + configSource)
	db, err := connectTargetDB(ctx, targetCfg)
	if err != nil {
		return fail(ipc, fmt.Errorf("Failed to connect to MitM database: %w", err))
	}
	defer db.Close()
	ipc.SendEvent("processing", "Connected to MitM database", 20)

	result, err := monitor(ctx, db, args)
	if err != nil {
		return fail(ipc, err)
	}
	for _, source := range result.initialized {
		ipc.SendAudit(fmt.Sprintf("Initialized cursor of %s; older entries are not reported", source))
	}
	if result.missedTruncated {
		ipc.SendAudit(fmt.Sprintf("WARNING: missed runs were only evaluated for the last %s", maxMissedWindow))
	}
	for _, program := range result.invalidCrons {
		ipc.SendAudit("WARNING: invalid cron expression, the scheduler never starts " + program)
	}
	ipc.SendAudit(result.summary())
	ipc.SendAudit(fmt.Sprintf("%s (%s) finished", appName, version))
	log.Printf("%s finished. %s", appName, result.summary())
	return nil
}

// monitorResult summarizes one run.
type monitorResult struct {
	initialized                            []string
	invalidCrons                           []string
	missedTruncated                        bool
	finishedRuns, failedRuns, hanging      int
	missedRuns, systemErrors, deadLetters  int
	securityEvents                         int
	mails, recoveries, bundled, suppressed int
	digest, heartbeat                      bool
}

func (r monitorResult) summary() string {
	daily := ""
	if r.digest {
		daily += ", daily digest"
	}
	if r.heartbeat {
		daily += ", heartbeat"
	}
	return fmt.Sprintf("Processed %d finished runs (%d failed), %d hanging runs, %d missed runs, %d system errors, "+
		"%d dead letters, %d security events; mails written: %d (recoveries: %d, bundled into summary: %d%s), suppressed: %d",
		r.finishedRuns, r.failedRuns, r.hanging, r.missedRuns, r.systemErrors, r.deadLetters, r.securityEvents,
		r.mails, r.recoveries, r.bundled, daily, r.suppressed)
}

// monitor collects the events of all enabled sources, writes the mail files and then commits
// cursors and alert state in one transaction.
func monitor(ctx context.Context, db *pgxpool.Pool, args MonitorArgs) (monitorResult, error) {
	var result monitorResult
	now, err := dbNow(ctx, db)
	if err != nil {
		return result, err
	}
	settled := now.Add(-settleDelay)
	cursors, err := loadCursors(ctx, db)
	if err != nil {
		return result, err
	}
	for _, source := range eventSources {
		if cursors[source].isNew {
			result.initialized = append(result.initialized, source)
		}
	}

	runsCfg := args.Sources.ProgramRuns
	var runs []finishedRun
	var events []Event
	failedByRun := map[int64]Event{}
	if enabled(runsCfg.Enabled) {
		all, err := collectFinishedRuns(ctx, db, cursors[sourceProgramRuns], settled)
		if err != nil {
			return result, err
		}
		for _, run := range all {
			if slices.Contains(runsCfg.ExcludeJobs, run.Job) {
				continue
			}
			runs = append(runs, run)
			if !run.Success {
				lines, err := runErrors(ctx, db, run.ID)
				if err != nil {
					return result, err
				}
				failedByRun[run.ID] = failedRunEvent(run, lines)
			}
		}
		hanging, err := collectHangingRuns(ctx, db, now, runsCfg.HangingMinutes)
		if err != nil {
			return result, err
		}
		for _, event := range hanging {
			if !slices.Contains(runsCfg.ExcludeJobs, event.JobName) {
				events = append(events, event)
				result.hanging++
			}
		}
	}
	if enabled(args.Sources.SystemLogs.Enabled) {
		systemEvents, err := collectSystemLogs(ctx, db, cursors[sourceSystemLogs], settled, args.Sources.SystemLogs)
		if err != nil {
			return result, err
		}
		events = append(events, systemEvents...)
		result.systemErrors = len(systemEvents)
	}
	if enabled(args.Sources.DeadLetterQueue.Enabled) {
		letters, err := collectDeadLetters(ctx, db, cursors[sourceDLQ], settled)
		if err != nil {
			return result, err
		}
		events = append(events, letters...)
		result.deadLetters = len(letters)
	}
	if enabled(args.Sources.MissedRuns.Enabled) {
		missed, err := collectMissedRuns(ctx, db, cursors[sourceMissedRuns], now, args.Sources.MissedRuns)
		if err != nil {
			return result, err
		}
		events = append(events, missed.events...)
		result.missedRuns, result.invalidCrons, result.missedTruncated = len(missed.events), missed.invalidCrons, missed.truncated
	}
	if args.Sources.AdminAuditLogs.Enabled {
		security, err := collectAdminAudit(ctx, db, cursors[sourceAdminAudit], settled, args.Sources.AdminAuditLogs)
		if err != nil {
			return result, err
		}
		events = append(events, security...)
		result.securityEvents = len(security)
	}
	result.finishedRuns, result.failedRuns = len(runs), len(failedByRun)

	fingerprints := make([]string, 0, len(failedByRun)+len(events))
	for _, event := range failedByRun {
		fingerprints = append(fingerprints, event.fingerprint())
	}
	for _, event := range events {
		fingerprints = append(fingerprints, event.fingerprint())
	}
	alerts, err := loadAlerts(ctx, db, fingerprints)
	if err != nil {
		return result, err
	}

	plan := newPlanner(args, now, alerts)
	// Finished runs in finish order: a failure opens an alert, a later success recovers it.
	// Immediate runs share one name, so their success must not recover other immediate runs.
	for _, run := range runs {
		switch {
		case !run.Success:
			plan.handle(failedByRun[run.ID])
		case run.Program:
			plan.recover(run.Job, fmt.Sprintf("%s/%d", sourceProgramRuns, run.ID), run.ID, run.Finished)
		}
	}
	for _, event := range events {
		plan.handle(event)
	}

	mails, bundled := plan.limit()
	daily, err := dailyMails(ctx, db, args, now, cursors, result.invalidCrons)
	if err != nil {
		return result, err
	}
	result.digest, result.heartbeat = daily.digest, daily.heartbeat
	mails = append(mails, daily.mails...)
	for _, mail := range mails {
		if err := writeMail(args.MailDir, mail); err != nil {
			return result, err
		}
	}
	if err := commitState(ctx, db, cursors, plan.dirtyAlerts()); err != nil {
		return result, err
	}
	result.mails, result.recoveries, result.bundled, result.suppressed = len(mails), plan.recovered, bundled, plan.suppressed
	return result, nil
}

// fail reports a fatal error as status event and job audit entry; the caller exits with code 1.
func fail(ipc *IPCClient, err error) error {
	ipc.SendEvent("failed", err.Error(), 0)
	ipc.SendAudit("ERROR: " + err.Error())
	return err
}

// --- MitM database ---------------------------------------------------------------

func loadTargetDBConfig() (TargetDBConfig, string, error) {
	var cfg TargetDBConfig
	source := "Environment Variables"
	sslMode := os.Getenv("MITM_DB_SSLMODE")
	if raw := os.Getenv("MITM_DB_CONFIG_JSON"); raw != "" {
		var full struct {
			DB struct {
				Host     string `json:"host"`
				Port     int    `json:"port"`
				User     string `json:"user"`
				Password string `json:"password"`
				Database string `json:"database"`
				SSLMode  bool   `json:"sslmode"`
			} `json:"db"`
		}
		if err := json.Unmarshal([]byte(raw), &full); err != nil {
			return cfg, source, fmt.Errorf("Failed to parse MitM database JSON config: %w", err)
		}
		cfg = TargetDBConfig{full.DB.Host, full.DB.Port, full.DB.User, full.DB.Password, full.DB.Database, ""}
		sslMode = strconv.FormatBool(full.DB.SSLMode)
		source = "JSON Config (MITM_DB_CONFIG_JSON)"
	} else {
		cfg.Host = os.Getenv("MITM_DB_HOST")
		cfg.Port, _ = strconv.Atoi(os.Getenv("MITM_DB_PORT"))
		cfg.User = os.Getenv("MITM_DB_USER")
		cfg.Password = os.Getenv("MITM_DB_PASSWORD")
		cfg.Database = os.Getenv("MITM_DB_NAME")
	}
	switch sslMode {
	case "", "false":
		cfg.SSLMode = "disable"
	case "true":
		cfg.SSLMode = "require"
	default:
		cfg.SSLMode = sslMode
	}
	if cfg.Host == "" {
		return cfg, source, errors.New("MitM database credentials not found in environment (MITM_DB_HOST or MITM_DB_CONFIG_JSON)")
	}
	return cfg, source, nil
}

func connectTargetDB(ctx context.Context, cfg TargetDBConfig) (*pgxpool.Pool, error) {
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:     "/" + cfg.Database,
		RawQuery: "sslmode=" + url.QueryEscape(cfg.SSLMode),
	}
	poolCfg, err := pgxpool.ParseConfig(dsn.String())
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = 4
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	return pgxpool.NewWithConfig(ctx, poolCfg)
}

// --- Scheduler IPC ---------------------------------------------------------------

func newIPCClient() *IPCClient {
	socketPath := os.Getenv("SCHEDULER_SOCKET_PATH")
	runID, err := strconv.Atoi(os.Getenv("RUN_ID"))
	if err != nil || socketPath == "" {
		return nil
	}
	return &IPCClient{SocketPath: socketPath, RunID: runID, Component: componentName}
}

func (c *IPCClient) SendEvent(status, message string, progress int) {
	if c == nil || c.SocketPath == "" {
		return
	}
	c.send(StatusEvent{RunID: c.RunID, Type: "status", Status: status, Message: message, Progress: progress})
}

func (c *IPCClient) SendAudit(message string) {
	if c == nil || c.SocketPath == "" {
		return
	}
	c.send(StatusEvent{RunID: c.RunID, Type: "audit", Component: c.Component, Message: message})
}

func (c *IPCClient) send(event StatusEvent) {
	conn, err := net.Dial("unix", c.SocketPath)
	if err != nil {
		log.Printf("[IPC ERROR] Failed to connect to scheduler socket: %v", err)
		return
	}
	defer conn.Close()
	data, _ := json.Marshal(event)
	_, _ = conn.Write(append(data, '\n'))
}

// fetchDBConfigFromScheduler requests get_credentials and returns only db_config_json.
func fetchDBConfigFromScheduler() (string, error) {
	runIDStr := os.Getenv("RUN_ID")
	socketPath := os.Getenv("SCHEDULER_SOCKET_PATH")
	if runIDStr == "" || socketPath == "" {
		return "", errors.New("not running under scheduler")
	}
	runID, err := strconv.Atoi(runIDStr)
	if err != nil {
		return "", err
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	data, _ := json.Marshal(map[string]any{"type": "get_credentials", "run_id": runID})
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		var resp struct {
			DBConfigJSON string `json:"db_config_json"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &resp); err == nil {
			return resp.DBConfigJSON, nil
		}
	}
	return "", errors.New("no response or invalid JSON from scheduler")
}
