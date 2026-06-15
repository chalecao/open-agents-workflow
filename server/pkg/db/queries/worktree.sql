-- =============================================================================
-- Worktree-related queries.
-- =============================================================================
--
-- Worktrees are git worktrees on the daemon host, identified by their absolute
-- filesystem path (which becomes comment.worktree_id when the agent posts a
-- comment). We don't model them as a separate table — the daemon is the
-- authoritative source for live state (branch / dirty / untracked), and the
-- server only needs to know the set of paths associated with an issue so the
-- issue-detail sidebar can list them.

-- name: ListIssueWorktreePaths :many
-- Distinct non-empty work_dir values from agent_task_queue rows for an issue.
-- Used by the issue-detail sidebar to render the worktree list. Excludes
-- NULL/empty so the sidebar never tries to render a "(no path)" placeholder;
-- a row whose worktree was GC'd is filtered out the same way as a row that
-- never set one.
SELECT DISTINCT work_dir
FROM agent_task_queue
WHERE issue_id = $1
  AND work_dir IS NOT NULL
  AND work_dir != ''
ORDER BY work_dir ASC;

-- name: ListIssueWorktreeTaskStats :many
-- Per-worktree aggregate task counts for the sidebar. One row per work_dir
-- with the agent count, last activity, and (most usefully) the latest task
-- status. The latest activity drives the "active / past" badge and the
-- sidebar's "click to expand" affordance; the latest status drives the
-- colored dot.
SELECT work_dir,
       COUNT(*) AS task_count,
       COUNT(DISTINCT agent_id) AS agent_count,
       MAX(GREATEST(COALESCE(completed_at, 'epoch'::timestamptz), COALESCE(started_at, 'epoch'::timestamptz), COALESCE(dispatched_at, 'epoch'::timestamptz), COALESCE(created_at, 'epoch'::timestamptz))) AS last_activity_at,
       (
         SELECT atq2.status
         FROM agent_task_queue atq2
         WHERE atq2.work_dir = agent_task_queue.work_dir
           AND atq2.issue_id = $1
         ORDER BY atq2.created_at DESC
         LIMIT 1
       ) AS latest_status,
       (
         SELECT atq3.id
         FROM agent_task_queue atq3
         WHERE atq3.work_dir = agent_task_queue.work_dir
           AND atq3.issue_id = $1
         ORDER BY atq3.created_at DESC
         LIMIT 1
       ) AS latest_task_id
FROM agent_task_queue
WHERE issue_id = $1
  AND work_dir IS NOT NULL
  AND work_dir != ''
GROUP BY work_dir
ORDER BY work_dir ASC;
