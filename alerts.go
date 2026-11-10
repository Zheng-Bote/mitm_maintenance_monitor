/**
 * SPDX-FileComment: MitM Maintenance Monitor - alert planning
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file alerts.go
 * @brief Events, fingerprints, repeat suppression, recovery, rate limit and routing.
 *        Pure logic without database access.
 * @version 0.1.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	severityError = "ERROR"
	severityInfo  = "INFO"
	timeLayout    = "2006-01-02 15:04:05 UTC"
)

var (
	uuidPattern       = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	digitsPattern     = regexp.MustCompile(`[0-9]+`)
	whitespacePattern = regexp.MustCompile(`\s+`)
)

// Event is one detected problem of a monitored source.
type Event struct {
	Source   string      // unique source entry, e.g. "program_runs/3732"
	Category string      // routing category: job, system, dlq
	Group    string      // fingerprint group: job name, component, error code, hanging run
	JobName  string      // job alerts only (recovery)
	Subject  string      // subject text without prefix and severity
	Fields   [][2]string // metadata lines (label, value)
	Errors   []string    // error texts; omitted with metadata_only
	ErrorKey string      // text that identifies "the same error"
	Time     time.Time
}

// Alert is the suppression state of one fingerprint (table monitor_alerts).
type Alert struct {
	Fingerprint     string
	Category        string
	JobName         string
	Summary         string
	FirstSeen       time.Time
	LastSeen        time.Time
	LastMailedAt    *time.Time
	SuppressedCount int
	IsOpen          bool
	dirty           bool
}

// Mail is one mail file to be written.
type Mail struct {
	Name     string // file name without extension
	To       []string
	Cc       []string
	Subject  string
	Severity string
	Category string
	Sources  []string
	Body     string
}

// normalizeError removes variable parts (numbers, UUIDs) so repeated errors share a fingerprint.
func normalizeError(text string) string {
	text = uuidPattern.ReplaceAllString(text, "<uuid>")
	text = digitsPattern.ReplaceAllString(text, "#")
	text = strings.TrimSpace(whitespacePattern.ReplaceAllString(text, " "))
	return truncate(text, 300)
}

func (e Event) fingerprint() string {
	hash := sha256.Sum256([]byte(e.Category + "|" + e.Group + "|" + normalizeError(e.ErrorKey)))
	return hex.EncodeToString(hash[:])
}

func truncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}

func mailName(sources []string) string {
	hash := sha256.Sum256([]byte(strings.Join(sources, "|")))
	return "mitm-monitor_" + hex.EncodeToString(hash[:12])
}

// planner turns events into mails and updates the alert state.
type planner struct {
	args       MonitorArgs
	now        time.Time
	alerts     map[string]*Alert
	mails      []Mail
	suppressed int
	recovered  int
}

func newPlanner(args MonitorArgs, now time.Time, alerts map[string]*Alert) *planner {
	if alerts == nil {
		alerts = map[string]*Alert{}
	}
	return &planner{args: args, now: now, alerts: alerts}
}

// handle writes a mail for a new alert or after the suppression window, otherwise counts it.
func (p *planner) handle(e Event) {
	fingerprint := e.fingerprint()
	alert := p.alerts[fingerprint]
	window := time.Duration(*p.args.SuppressRepeatMinutes) * time.Minute

	switch {
	case alert == nil || !alert.IsOpen:
		alert = &Alert{Fingerprint: fingerprint, Category: e.Category, JobName: e.JobName, FirstSeen: e.Time, IsOpen: true}
		p.alerts[fingerprint] = alert
		p.mailEvent(e, alert)
	case alert.LastMailedAt == nil || !p.now.Before(alert.LastMailedAt.Add(window)):
		p.mailEvent(e, alert)
	default:
		alert.SuppressedCount++
		p.suppressed++
	}
	alert.Summary = truncate(e.Subject, 200)
	if e.Time.After(alert.LastSeen) {
		alert.LastSeen = e.Time
	}
	alert.dirty = true
}

func (p *planner) mailEvent(e Event, alert *Alert) {
	var body strings.Builder
	writeFields(&body, e.Fields)
	if alert.SuppressedCount > 0 && alert.LastMailedAt != nil {
		fmt.Fprintf(&body, "Suppressed:  %d further occurrences since %s\n",
			alert.SuppressedCount, alert.LastMailedAt.UTC().Format(timeLayout))
	}
	p.writeErrors(&body, e.Errors)
	p.mails = append(p.mails, p.newMail(e.Category, severityError, e.Subject, []string{e.Source}, body.String()))

	now := p.now
	alert.LastMailedAt = &now
	alert.SuppressedCount = 0
}

// recover closes the open job alerts of a job after a successful run and writes a recovery mail
// if one of them had been mailed.
func (p *planner) recover(job string, runSource string, runID int64, finished time.Time) {
	fingerprints := make([]string, 0)
	for fingerprint, alert := range p.alerts {
		if alert.IsOpen && alert.Category == categoryJob && alert.JobName == job {
			fingerprints = append(fingerprints, fingerprint)
		}
	}
	sort.Strings(fingerprints)

	mailed, suppressed := false, 0
	var since time.Time
	for _, fingerprint := range fingerprints {
		alert := p.alerts[fingerprint]
		alert.IsOpen = false
		alert.dirty = true
		if alert.LastMailedAt != nil {
			mailed = true
			suppressed += alert.SuppressedCount
			if since.IsZero() || alert.FirstSeen.Before(since) {
				since = alert.FirstSeen
			}
		}
	}
	if !mailed || !*p.args.RecoveryMail {
		return
	}

	var body strings.Builder
	fields := [][2]string{
		{"Job", job},
		{"Run ID", fmt.Sprint(runID)},
		{"Finished", finished.UTC().Format(timeLayout)},
		{"Since", since.UTC().Format(timeLayout)},
	}
	if suppressed > 0 {
		fields = append(fields, [2]string{"Suppressed", fmt.Sprintf("%d occurrences without mail", suppressed)})
	}
	writeFields(&body, fields)
	body.WriteString("\nThe job finished successfully again; its open alerts are closed.\n")
	subject := fmt.Sprintf("Job %s recovered (RunID %d)", job, runID)
	p.mails = append(p.mails, p.newMail(categoryJob, severityInfo, subject, []string{"recovery/" + runSource}, body.String()))
	p.recovered++
}

func (p *planner) writeErrors(body *strings.Builder, errors []string) {
	if p.args.MetadataOnly {
		body.WriteString("\nError details are omitted (metadata_only).\n")
		return
	}
	if len(errors) == 0 {
		return
	}
	body.WriteString("\nErrors:\n")
	for _, text := range errors {
		body.WriteString("- " + truncate(strings.TrimSpace(text), p.args.MaxMessageChars) + "\n")
	}
}

func (p *planner) newMail(category, severity, subject string, sources []string, body string) Mail {
	to, cc := p.args.recipients(category)
	if p.args.FrontendURL != "" {
		body += "\nDetails: " + p.args.FrontendURL + "\n"
	}
	return Mail{
		Name:     mailName(sources),
		To:       to,
		Cc:       cc,
		Subject:  truncate(fmt.Sprintf("%s[%s] %s", p.args.SubjectPrefix, severity, subject), 250),
		Severity: severity,
		Category: category,
		Sources:  sources,
		Body:     body,
	}
}

// limit keeps the first max mails and bundles the rest into one summary mail.
func (p *planner) limit() (mails []Mail, bundled int) {
	max := p.args.MaxMailsPerRun
	if len(p.mails) <= max {
		return p.mails, 0
	}
	rest := p.mails[max:]
	var to, cc, sources []string
	var body strings.Builder
	fmt.Fprintf(&body, "%d further alerts exceeded max_mails_per_run (%d):\n\n", len(rest), max)
	for _, mail := range rest {
		to = appendUnique(to, mail.To...)
		cc = appendUnique(cc, mail.Cc...)
		sources = append(sources, mail.Sources...)
		fmt.Fprintf(&body, "- %s (%s)\n", mail.Subject, strings.Join(mail.Sources, ", "))
	}
	summary := Mail{
		Name:     mailName(append([]string{"summary"}, sources...)),
		To:       to,
		Cc:       cc,
		Subject:  fmt.Sprintf("%s[%s] %d further alerts", p.args.SubjectPrefix, severityError, len(rest)),
		Severity: severityError,
		Category: "summary",
		Sources:  sources,
		Body:     body.String(),
	}
	if p.args.FrontendURL != "" {
		summary.Body += "\nDetails: " + p.args.FrontendURL + "\n"
	}
	return append(append([]Mail{}, p.mails[:max]...), summary), len(rest)
}

func (p *planner) dirtyAlerts() []*Alert {
	var alerts []*Alert
	for _, alert := range p.alerts {
		if alert.dirty {
			alerts = append(alerts, alert)
		}
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Fingerprint < alerts[j].Fingerprint })
	return alerts
}

func writeFields(body *strings.Builder, fields [][2]string) {
	for _, field := range fields {
		fmt.Fprintf(body, "%-12s %s\n", field[0]+":", field[1])
	}
}
