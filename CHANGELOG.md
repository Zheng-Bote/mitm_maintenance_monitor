# Changelog

All notable changes to the `mitm_maintenance_monitor` component will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.1.1] - 2026-11-10

### Fixed
- Immediate runs (`program_id` NULL, e.g. file uploads) made the `program_runs` query fail (`cannot scan NULL`); they are now reported as job `Immediate_Trigger`, like in the scheduler.
- A successful immediate run no longer recovers the alerts of other immediate runs.

### Added
- Read-only integration test (`go test -tags integration`) for the source queries against a real MitM database.

## [v0.1.0] - 2026-11-10

### Added
- **Maintenance Monitor (phase 1)**: scheduled job that detects failed and hanging job runs (`program_runs`), `system_logs` errors and new unresolved dead letters and writes them as `*.SendMail` files (header-block format) into the outbox of `mitm_maintenance_sendmail` (issue #1).
- Cursor per source in `monitor_state` (first run starts at the current position), suppression state per fingerprint in `monitor_alerts` (migration `007_monitor_state.sql` in mitm-2).
- Repeat suppression (`suppress_repeat_minutes`), recovery mails, `max_mails_per_run` with summary mail, routing by category, `max_message_chars`, `metadata_only` and `frontend_url`.
- Atomic mail files (`*.tmp` → `*.SendMail`, mode `0600`) with deterministic names; mail files are written before cursors and alert state are committed.
