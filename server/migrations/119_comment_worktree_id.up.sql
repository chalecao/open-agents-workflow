-- 119_comment_worktree_id.up.sql
--
-- Stamps the absolute worktree path on agent-authored comments so the
-- issue sidebar can cross-link from the agent card header into the
-- per-worktree file tree / diff. The path itself comes from
-- agent_task_queue.work_dir (the on-disk worktree the agent was using
-- for the task); see migration 117 for the worktree plumbing.
--
-- Nullable: user / system / older agent comments that have no worktree
-- association stay NULL. The handler reads the column as pgtype.Text
-- with `omitempty` so the response shape stays identical for callers
-- that don't need it.
ALTER TABLE comment ADD COLUMN IF NOT EXISTS worktree_id TEXT;

-- Partial index — only the agent-authored rows that actually carry a
-- worktree id are ever grouped/queried. The sidebar query is
-- `WHERE issue_id = $1 AND worktree_id IS NOT NULL GROUP BY
-- worktree_id`, and the per-card cross-link is `worktree_id = $1`.
-- A partial index is dramatically smaller than a full index on the
-- whole comment table.
CREATE INDEX IF NOT EXISTS idx_comment_worktree_id
    ON comment (worktree_id)
    WHERE worktree_id IS NOT NULL;
