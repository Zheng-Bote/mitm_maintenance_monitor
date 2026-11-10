//go:build integration

// SPDX-License-Identifier: Apache-2.0

// Read-only integration test against a real MitM database:
//
//	MITM_DB_HOST=... MITM_DB_PORT=... MITM_DB_USER=... MITM_DB_PASSWORD=... MITM_DB_NAME=... \
//	  go test -tags integration -run TestReadOnlySources -v ./...
//
// It runs the source queries and the first-run initialization in memory and writes nothing,
// so it works without migration 007.

package main

import (
	"context"
	"testing"
	"time"
)

func TestReadOnlySources(t *testing.T) {
	ctx := context.Background()
	cfg, _, err := loadTargetDBConfig()
	if err != nil {
		t.Fatal(err)
	}
	db, err := connectTargetDB(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now, err := dbNow(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * 365 * 24 * time.Hour)

	runs, err := collectFinishedRuns(ctx, db, &cursor{Source: sourceProgramRuns, LastTS: &old, LastID: "0"}, now.Add(-settleDelay))
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, run := range runs {
		if !run.Success {
			failed++
			lines, err := runErrors(ctx, db, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			e := failedRunEvent(run, lines)
			t.Logf("failed run %d job=%q exit=%v errorLines=%d subject=%q", run.ID, run.Job, run.ExitCode, len(lines), e.Subject)
		}
	}
	t.Logf("finished runs=%d failed=%d", len(runs), failed)

	hanging, err := collectHangingRuns(ctx, db, now, 120)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("hanging runs=%d", len(hanging))
	for _, e := range hanging {
		t.Logf("hanging: %s", e.Subject)
	}

	sys, err := collectSystemLogs(ctx, db, &cursor{Source: sourceSystemLogs, LastID: "0"}, now.Add(-settleDelay),
		SystemLogsConfig{MinLevel: "ERROR", ExcludeComponents: []string{componentName}})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	for _, e := range sys {
		groups[e.fingerprint()]++
	}
	t.Logf("system errors=%d distinct fingerprints=%d", len(sys), len(groups))
	for i, e := range sys {
		if i >= 5 {
			break
		}
		t.Logf("system: %s", e.Subject)
	}

	dlq, err := collectDeadLetters(ctx, db, &cursor{Source: sourceDLQ, LastTS: &old}, now.Add(-settleDelay))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dead letters=%d", len(dlq))

	// First-run initialization paths (in memory only).
	for _, c := range []*cursor{{Source: sourceProgramRuns, isNew: true}, {Source: sourceSystemLogs, isNew: true}, {Source: sourceDLQ, isNew: true}} {
		var err error
		switch c.Source {
		case sourceProgramRuns:
			_, err = collectFinishedRuns(ctx, db, c, now.Add(-settleDelay))
		case sourceSystemLogs:
			_, err = collectSystemLogs(ctx, db, c, now.Add(-settleDelay), SystemLogsConfig{MinLevel: "ERROR"})
		case sourceDLQ:
			_, err = collectDeadLetters(ctx, db, c, now.Add(-settleDelay))
		}
		if err != nil || !c.dirty {
			t.Fatalf("init %s: %v", c.Source, err)
		}
		t.Logf("init %s -> last_id=%q", c.Source, c.LastID)
	}
}
