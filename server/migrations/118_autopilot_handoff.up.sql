-- =============================================================================
-- Handoff autopilot: source agent comment → rule evaluation → @ target agent
-- =============================================================================
--
-- Adds a new autopilot execution_mode 'handoff' that watches a source agent's
-- comments on an issue, evaluates a user-configured rule set against the
-- comment text + issue metadata, and on a match posts a comment that
-- @-mentions a target agent (re-using the existing enqueueMentionedAgentTasks
-- pipeline to dispatch the work).
--
-- Two new storage fields:
--
--   1. issue.handoff_data — a JSONB blob agents use to package structured
--      handoff payloads (what was done, what the next agent should know).
--      Always a JSON object when present, with the same primitive-value rule
--      as issue.metadata. The template language {{handoff_data}} interpolates
--      the whole object as a JSON string; finer-grained key access is left
--      to the configured rules / template text the user composes.
--
--   2. autopilot.{handoff_*} — five columns that fully describe a handoff
--      autopilot. All NULLable so existing create_issue / run_only rows are
--      unaffected (and a handoff row that hasn't been fully configured yet
--      is queryable, not rejected at INSERT time).
--
-- CHECK constraints guard the operator enum (only 'all' / 'any') so a bad
-- write at the SQL layer is caught even if the handler skips validation.

ALTER TABLE issue ADD COLUMN IF NOT EXISTS handoff_data JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE issue ADD CONSTRAINT issue_handoff_data_is_object
    CHECK (jsonb_typeof(handoff_data) = 'object');
ALTER TABLE issue ADD CONSTRAINT issue_handoff_data_size_limit
    CHECK (pg_column_size(handoff_data) <= 8192);
-- GIN with jsonb_path_ops mirrors the metadata index — a handoff rules engine
-- that wants to query "does this issue have a handoff_data.foo key?" uses
-- the same containment operator (@>) without paying for a larger index.
CREATE INDEX IF NOT EXISTS idx_issue_handoff_data_gin ON issue USING GIN (handoff_data jsonb_path_ops);

-- Autopilot execution_mode gains the 'handoff' variant. Drop & re-add the
-- CHECK because PG does not allow in-place enum-value edits on a CHECK.
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only', 'handoff'));

ALTER TABLE autopilot
    ADD COLUMN IF NOT EXISTS handoff_source_agent_id UUID REFERENCES agent(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS handoff_target_agent_id UUID REFERENCES agent(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS handoff_rules JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS handoff_rules_operator TEXT NOT NULL DEFAULT 'all',
    ADD COLUMN IF NOT EXISTS handoff_comment_template TEXT;

-- Guard the operator enum at the SQL layer. The handler enforces the same
-- set in Go; the CHECK is defense in depth for direct SQL writes.
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_operator_check;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_handoff_rules_operator_check
    CHECK (handoff_rules_operator IN ('all', 'any'));

-- Reasonable size limits on the rules blob. The handler also validates
-- structure; the DB just caps the wire-format size so a misbehaving writer
-- cannot stuff 100MB of "rules" into a row.
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_size_limit;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_handoff_rules_size_limit
    CHECK (pg_column_size(handoff_rules) <= 16384);

-- Rules blob must be an array — every rule evaluation loops over the array,
-- and a non-array would crash the loop. Empty array is valid ("never match")
-- and the default.
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_handoff_rules_is_array;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_handoff_rules_is_array
    CHECK (jsonb_typeof(handoff_rules) = 'array');

-- Index for the comment-event hot path: when a comment is created we need to
-- find active handoff autopilots in the workspace whose source_agent matches
-- the comment's author. The partial index keeps it small (only handoff rows,
-- only active status).
CREATE INDEX IF NOT EXISTS idx_autopilot_handoff_source_active
    ON autopilot(workspace_id, handoff_source_agent_id)
    WHERE execution_mode = 'handoff' AND status = 'active';
