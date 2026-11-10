# Changelog

All notable changes to the `mitm_maintenance_monitor` component will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.2.2] - 2026-11-10

### Fixed
- Recipient addresses (`routing`, `digest`, `heartbeat`) are validated with the rule of `mitm_maintenance_sendmail` (domain must contain a dot): an address such as `ops@localhost` is rejected at startup instead of ending up as `*.failed` file in the outbox.

## [v0.2.1] - 2026-11-10

### Changed
- Default `sources.admin_audit_logs.actions` follow mitm-http-server 3.3.0 ([core-layer #86](https://github.com/Zheng-Bote/mitm_core-layer/issues/86)): `ASSIGN_ROLES`, `RESTORE_CONFIG`, `KEY_ROTATION_SUCCESS`, `UPSERT_CREDENTIAL`, `DELETE_CREDENTIAL`, `UPSERT_TARGET_CREDENTIAL`, `DELETE_TARGET_CREDENTIAL`, `FORBIDDEN_ACCESS`, `USER_LOGIN_FAILED`, `BOOTSTRAP_ADMIN_REGISTERED` (matching stays case-insensitive).

## [v0.2.0] - 2026-11-10

Phase 2 of [#1](https://github.com/Zheng-Bote/mitm_maintenance_monitor/issues/1).

### Added
- **Missed runs** (`sources.missed_runs`, enabled by default): enabled scheduled programs without a run at a cron time within `grace_minutes`; cron expressions are evaluated with the semantics of the scheduler's Rust crate `cron` 0.12 (UTC, weekdays 1–7 with Sunday = 1, day of month AND day of week), verified against the crate. Running previous runs, program changes, invalid cron expressions and monitor outages (24 h window) are handled.
- **Security events** (`sources.admin_audit_logs`, opt-in): configurable critical actions (case-insensitive), category `security`, severity `SECURITY`, only allowlisted `details` fields.
- **Daily digest** (opt-in): runs per job, grouped `WARNING:` job audits and `WARN` system logs, DLQ backlog, transformation errors and open alerts of the last 24 hours.
- **Heartbeat** (opt-in): daily proof-of-life mail with run statistics, open alerts, invalid cron expressions and source positions.
- Events carry their own severity (`ERROR`, `INFO`, `SECURITY`).

## [v0.1.1] - 2026-11-10

### Fixed
- Immediate runs (`program_id` NULL, e.g. file uploads) made the `program_runs` query fail (`cannot scan NULL`); they are now reported as job `Immediate_Trigger`, like in the scheduler.
- A successful immediate run no longer recovers the alerts of other immediate runs.
- Recovery mails: label `Failing since` shortened to `Since`, so the field column stays aligned.

### Added
- Read-only integration test (`go test -tags integration`) for the source queries against a real MitM database.

## [v0.1.0] - 2026-11-10

### Added
- **Maintenance Monitor (phase 1)**: scheduled job that detects failed and hanging job runs (`program_runs`), `system_logs` errors and new unresolved dead letters and writes them as `*.SendMail` files (header-block format) into the outbox of `mitm_maintenance_sendmail` (issue #1).
- Cursor per source in `monitor_state` (first run starts at the current position), suppression state per fingerprint in `monitor_alerts` (migration `007_monitor_state.sql` in mitm-2).
- Repeat suppression (`suppress_repeat_minutes`), recovery mails, `max_mails_per_run` with summary mail, routing by category, `max_message_chars`, `metadata_only` and `frontend_url`.
- Atomic mail files (`*.tmp` → `*.SendMail`, mode `0600`) with deterministic names; mail files are written before cursors and alert state are committed.
