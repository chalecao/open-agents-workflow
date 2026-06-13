-- name: ListOnlineRuntimesByProvider :many
-- Returns every online agent_runtime whose `provider` column matches
-- the supplied value. Used by the server-side LLM execution worker
-- to find the openai-http runtimes it should poll for tasks. The
-- 'online' filter is the same one the daemon heartbeats flip;
-- offline runtimes are ignored even if a task is queued against
-- them (FailTasksForOfflineRuntimes handles that case on the
-- timeout path).
SELECT
    id, workspace_id, daemon_id, name, runtime_mode, provider,
    status, device_info, metadata, last_seen_at, created_at,
    updated_at, owner_id, legacy_daemon_id, visibility
FROM agent_runtime
WHERE provider = $1 AND status = 'online'
ORDER BY created_at ASC;

-- name: ListRuntimesByProvider :many
-- Same shape as ListOnlineRuntimesByProvider but without the status
-- filter. Used by the worker keep-alive pass (server/internal/llmexec)
-- so an offline openai-http runtime whose endpoint has come back
-- reachable (e.g. user fixed a typo in base_url) is flipped back to
-- online without requiring a re-save of the LLM provider. Kept as a
-- separate query so the task-claiming hot path can keep the
-- status='online' index predicate.
SELECT
    id, workspace_id, daemon_id, name, runtime_mode, provider,
    status, device_info, metadata, last_seen_at, created_at,
    updated_at, owner_id, legacy_daemon_id, visibility
FROM agent_runtime
WHERE provider = $1
ORDER BY created_at ASC;
