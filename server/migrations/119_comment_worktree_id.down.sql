-- 119_comment_worktree_id.down.sql
--
-- Drops the worktree_id column and its partial index. The column is
-- pure metadata — removing it does not break any existing comment
-- content, and the worktree sidebar falls back to deriving paths from
-- agent_task_queue.work_dir.
DROP INDEX IF EXISTS idx_comment_worktree_id;
ALTER TABLE comment DROP COLUMN IF EXISTS worktree_id;
