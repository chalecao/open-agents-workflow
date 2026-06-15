-- 120_autopilot_run_handoff_source.up.sql
--
-- Migration 118 added the 'handoff' execution_mode and the handoff columns
-- on autopilot / autopilot_run-related plumbing, but did NOT extend the
-- autopilot_run.source CHECK constraint to allow the 'handoff' source
-- value. The handoff dispatcher in service/autopilot.go writes
-- `Source: "handoff"` to autopilot_run when a source agent's comment
-- matches a handoff autopilot's rule set — without this constraint
-- update, every handoff run fails at INSERT time with
--   ERROR: new row for relation "autopilot_run" violates check constraint
--   "autopilot_run_source_check"
-- and the dispatch is silently swallowed (only a slog.Warn surfaces in the
-- server log, no UI signal). The fix is to extend the allowed source set.
--
-- Reproducer:
--   INSERT INTO autopilot_run (autopilot_id, source, status)
--   VALUES (<handoff-autopilot-uuid>, 'handoff', 'running');
-- pre-fix: constraint violation.
-- post-fix: row inserted, run can be tracked like any other autopilot run.

ALTER TABLE autopilot_run DROP CONSTRAINT IF EXISTS autopilot_run_source_check;
ALTER TABLE autopilot_run ADD CONSTRAINT autopilot_run_source_check
    CHECK (source IN ('schedule', 'manual', 'webhook', 'api', 'handoff'));
