/**
 * SPDX-FileComment: MitM Maintenance Monitor - job arguments
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file config.go
 * @brief JSON job arguments of the monitor: defaults and validation.
 * @version 0.2.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strings"
)

// Alert categories used for routing.
const (
	categoryJob      = "job"
	categorySystem   = "system"
	categoryDLQ      = "dlq"
	categorySecurity = "security"
	categoryAny      = "*"
)

// defaultAdminActions are the admin_audit_logs actions reported by default (as written by mitm_http-server).
var defaultAdminActions = []string{"assign_roles", "RESTORE_CONFIG", "key_rotation_success", "DELETE_CREDENTIAL",
	"DELETE_TARGET_CREDENTIAL", "FORBIDDEN_ACCESS", "BOOTSTRAP_ADMIN_REGISTERED"}

// systemLevels are the system_logs levels in ascending order.
var systemLevels = []string{"DEBUG", "INFO", "WARN", "WARNING", "ERROR"}

// MonitorArgs are the job arguments passed by the scheduler as JSON in os.Args[1].
type MonitorArgs struct {
	MailDir               string        `json:"mail_dir"`
	SubjectPrefix         string        `json:"subject_prefix"`
	Sources               SourcesConfig `json:"sources"`
	Routing               []RoutingRule `json:"routing"`
	SuppressRepeatMinutes *int          `json:"suppress_repeat_minutes"`
	RecoveryMail          *bool         `json:"recovery_mail"`
	MaxMailsPerRun        int           `json:"max_mails_per_run"`
	MaxMessageChars       int           `json:"max_message_chars"`
	MetadataOnly          bool          `json:"metadata_only"`
	FrontendURL           string        `json:"frontend_url"`
	Digest                DailyMail     `json:"digest"`
	Heartbeat             DailyMail     `json:"heartbeat"`
}

// DailyMail configures a mail written once per day at HourUTC (digest, heartbeat).
type DailyMail struct {
	Enabled bool     `json:"enabled"`
	HourUTC *int     `json:"hour_utc"`
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
}

// SourcesConfig enables and configures the monitored sources.
type SourcesConfig struct {
	ProgramRuns     ProgramRunsConfig `json:"program_runs"`
	MissedRuns      MissedRunsConfig  `json:"missed_runs"`
	SystemLogs      SystemLogsConfig  `json:"system_logs"`
	DeadLetterQueue DLQConfig         `json:"dead_letter_queue"`
	AdminAuditLogs  AdminAuditConfig  `json:"admin_audit_logs"`
}

// MissedRunsConfig: enabled scheduled programs without a run at their cron times.
type MissedRunsConfig struct {
	Enabled      *bool    `json:"enabled"`
	GraceMinutes int      `json:"grace_minutes"`
	ExcludeJobs  []string `json:"exclude_jobs"`
}

// AdminAuditConfig: admin_audit_logs entries with a critical action (category security). Disabled by default.
type AdminAuditConfig struct {
	Enabled      bool     `json:"enabled"`
	Actions      []string `json:"actions"`
	DetailFields []string `json:"detail_fields"`
}

type ProgramRunsConfig struct {
	Enabled        *bool    `json:"enabled"`
	HangingMinutes int      `json:"hanging_minutes"`
	ExcludeJobs    []string `json:"exclude_jobs"`
}

type SystemLogsConfig struct {
	Enabled           *bool    `json:"enabled"`
	MinLevel          string   `json:"min_level"`
	ExcludeComponents []string `json:"exclude_components"`
}

type DLQConfig struct {
	Enabled *bool `json:"enabled"`
}

// RoutingRule maps an alert category to recipients.
type RoutingRule struct {
	Category string   `json:"category"`
	To       []string `json:"to"`
	Cc       []string `json:"cc"`
}

func enabled(flag *bool) bool { return flag == nil || *flag }

func parseArgs(raw string) (MonitorArgs, error) {
	var args MonitorArgs
	if strings.TrimSpace(raw) == "" {
		return args, errors.New("monitor arguments are required (JSON in the first argument)")
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return args, fmt.Errorf("Failed to parse monitor arguments: %w", err)
	}

	if args.SubjectPrefix == "" {
		args.SubjectPrefix = "[MitM]"
	}
	if args.SuppressRepeatMinutes == nil {
		minutes := 240
		args.SuppressRepeatMinutes = &minutes
	}
	if args.RecoveryMail == nil {
		recovery := true
		args.RecoveryMail = &recovery
	}
	if args.MaxMailsPerRun == 0 {
		args.MaxMailsPerRun = 20
	}
	if args.MaxMessageChars == 0 {
		args.MaxMessageChars = 500
	}
	if args.Sources.ProgramRuns.HangingMinutes == 0 {
		args.Sources.ProgramRuns.HangingMinutes = 120
	}
	if args.Sources.MissedRuns.GraceMinutes == 0 {
		args.Sources.MissedRuns.GraceMinutes = 30
	}
	if args.Sources.AdminAuditLogs.Actions == nil {
		args.Sources.AdminAuditLogs.Actions = defaultAdminActions
	}
	for _, daily := range []*DailyMail{&args.Digest, &args.Heartbeat} {
		if daily.HourUTC == nil {
			hour := 6
			daily.HourUTC = &hour
		}
	}
	if args.Sources.SystemLogs.MinLevel == "" {
		args.Sources.SystemLogs.MinLevel = "ERROR"
	}
	args.Sources.SystemLogs.MinLevel = strings.ToUpper(args.Sources.SystemLogs.MinLevel)
	if args.Sources.SystemLogs.MinLevel == "WARNING" {
		args.Sources.SystemLogs.MinLevel = "WARN"
	}
	if args.Sources.SystemLogs.ExcludeComponents == nil {
		args.Sources.SystemLogs.ExcludeComponents = []string{componentName}
	}

	if strings.TrimSpace(args.MailDir) == "" {
		return args, errors.New("mail_dir is required in the monitor arguments")
	}
	if info, err := os.Stat(args.MailDir); err != nil || !info.IsDir() {
		return args, fmt.Errorf("mail_dir %q is not an accessible directory", args.MailDir)
	}
	switch {
	case *args.SuppressRepeatMinutes < 0:
		return args, errors.New("suppress_repeat_minutes must not be negative")
	case args.MaxMailsPerRun < 1:
		return args, errors.New("max_mails_per_run must be at least 1")
	case args.MaxMessageChars < 50:
		return args, errors.New("max_message_chars must be at least 50")
	case args.Sources.ProgramRuns.HangingMinutes < 1:
		return args, errors.New("sources.program_runs.hanging_minutes must be at least 1")
	case args.Sources.MissedRuns.GraceMinutes < 1:
		return args, errors.New("sources.missed_runs.grace_minutes must be at least 1")
	case args.Sources.AdminAuditLogs.Enabled && len(args.Sources.AdminAuditLogs.Actions) == 0:
		return args, errors.New("sources.admin_audit_logs.actions must not be empty")
	case levelsFrom(args.Sources.SystemLogs.MinLevel) == nil:
		return args, fmt.Errorf("sources.system_logs.min_level %q is unknown (DEBUG, INFO, WARN, ERROR)", args.Sources.SystemLogs.MinLevel)
	case strings.ContainsAny(args.SubjectPrefix, "\r\n"):
		return args, errors.New("subject_prefix must not contain line breaks")
	}

	for i, rule := range args.Routing {
		if rule.Category == "" {
			return args, fmt.Errorf("routing[%d].category is required", i)
		}
		if len(rule.To) == 0 {
			return args, fmt.Errorf("routing[%d].to needs at least one recipient", i)
		}
		for _, address := range append(append([]string{}, rule.To...), rule.Cc...) {
			if err := validateAddress(address); err != nil {
				return args, fmt.Errorf("routing[%d]: %w", i, err)
			}
		}
	}
	for name, daily := range map[string]DailyMail{"digest": args.Digest, "heartbeat": args.Heartbeat} {
		if !daily.Enabled {
			continue
		}
		if *daily.HourUTC < 0 || *daily.HourUTC > 23 {
			return args, fmt.Errorf("%s.hour_utc must be between 0 and 23", name)
		}
		if len(daily.To) == 0 {
			return args, fmt.Errorf("%s.to needs at least one recipient", name)
		}
		for _, address := range append(append([]string{}, daily.To...), daily.Cc...) {
			if err := validateAddress(address); err != nil {
				return args, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	for _, category := range args.enabledCategories() {
		if to, _ := args.recipients(category); len(to) == 0 {
			return args, fmt.Errorf("no routing rule for category %q (add a rule for it or for %q)", category, categoryAny)
		}
	}
	return args, nil
}

// validateAddress accepts a plain e-mail address without display name.
func validateAddress(address string) error {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Name != "" || parsed.Address != address || strings.ContainsAny(address, ",\r\n") {
		return fmt.Errorf("invalid e-mail address %q", address)
	}
	return nil
}

func (a MonitorArgs) enabledCategories() []string {
	var categories []string
	if enabled(a.Sources.ProgramRuns.Enabled) || enabled(a.Sources.MissedRuns.Enabled) {
		categories = append(categories, categoryJob)
	}
	if a.Sources.AdminAuditLogs.Enabled {
		categories = append(categories, categorySecurity)
	}
	if enabled(a.Sources.SystemLogs.Enabled) {
		categories = append(categories, categorySystem)
	}
	if enabled(a.Sources.DeadLetterQueue.Enabled) {
		categories = append(categories, categoryDLQ)
	}
	return categories
}

// recipients resolves To and Cc of a category: all rules of the category, else all "*" rules.
func (a MonitorArgs) recipients(category string) (to, cc []string) {
	collect := func(match string) {
		for _, rule := range a.Routing {
			if rule.Category == match {
				to = appendUnique(to, rule.To...)
				cc = appendUnique(cc, rule.Cc...)
			}
		}
	}
	collect(category)
	if len(to) == 0 {
		collect(categoryAny)
	}
	return to, cc
}

// levelsFrom returns all system_logs levels at or above min, or nil if min is unknown.
func levelsFrom(min string) []string {
	for i, level := range systemLevels {
		if level == min {
			return systemLevels[i:]
		}
	}
	return nil
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, existing := range list {
			if strings.EqualFold(existing, value) {
				found = true
				break
			}
		}
		if !found {
			list = append(list, value)
		}
	}
	return list
}
