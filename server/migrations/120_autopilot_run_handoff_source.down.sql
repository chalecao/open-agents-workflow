-- Reverse 120_autopilot_run_handoff_source. Restore the original four-value
-- source enum. Safe because the migration that added 'handoff' is the
-- only thing that could have written 'handoff' rows, and a rollback to
-- 119 is expected to drop the handoff execution_mode anyway.
--
-- Note: this down migration will fail if any handoff runs are still on
-- disk (the original four-value CHECK rejects 'handoff'). The standard
-- down-roll pattern is to truncate autopilot_run first; we keep that as
-- an operator decision rather than encoding it here.

ALTER TABLE autopilot_run DROP CONSTRAINT IF EXISTS autopilot_run_source_check;
ALTER TABLE autopilot_run ADD CONSTRAINT autopilot_run_source_check
    CHECK (source IN ('schedule', 'manual', 'webhook', 'api'));
