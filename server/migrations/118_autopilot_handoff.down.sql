-- Reverse 118_autopilot_handoff. Order matters: drop CHECK constraints that
-- reference the handoff columns before dropping the columns themselves,
-- and drop the index before the table changes that would make it invalid.

DROP INDEX IF EXISTS idx_autopilot_handoff_source_active;

ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_is_array;
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_size_limit;
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_operator_check;

ALTER TABLE autopilot
    DROP COLUMN IF EXISTS handoff_comment_template,
    DROP COLUMN IF EXISTS handoff_rules_operator,
    DROP COLUMN IF EXISTS handoff_rules,
    DROP COLUMN IF EXISTS handoff_target_agent_id,
    DROP COLUMN IF EXISTS handoff_source_agent_id;

ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only'));

DROP INDEX IF EXISTS idx_issue_handoff_data_gin;
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_handoff_data_size_limit;
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_handoff_data_is_object;
ALTER TABLE issue DROP COLUMN IF EXISTS handoff_data;
