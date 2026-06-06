-- name: CreateLLMProvider :one
-- Inserts a workspace-level LLM provider and returns the row. The
-- companion agent_runtime row (provider='openai-http', runtime_mode='cloud')
-- is created in the same transaction by the handler so that the unique
-- (workspace_id, name) constraint on llm_provider and the unique
-- (workspace_id, daemon_id, provider) on agent_runtime are written
-- atomically — a half-insert would leave the UI with a provider that
-- doesn't have a runtime, or vice versa, and there is no recovery path
-- for that mismatch short of dropping the orphaned row.
INSERT INTO llm_provider (
    workspace_id,
    name,
    base_url,
    api_key_encrypted,
    model_name,
    runtime_id,
    created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: GetLLMProvider :one
SELECT * FROM llm_provider
WHERE id = $1;

-- name: GetLLMProviderForWorkspace :one
SELECT * FROM llm_provider
WHERE id = $1 AND workspace_id = $2;

-- name: GetLLMProviderByRuntime :one
-- Used by the server-side LLM execution worker: given the runtime_id
-- from a claimed task, look up the encrypted provider config (and
-- through the FK on runtime, the workspace). Returns the row so the
-- worker can decrypt the API key and issue the HTTP call.
SELECT * FROM llm_provider
WHERE runtime_id = $1;

-- name: ListLLMProvidersByWorkspace :many
SELECT * FROM llm_provider
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: UpdateLLMProvider :one
-- Partial update — every column is optional via COALESCE. NULL means
-- "leave the existing value alone". The handler validates user input
-- before this query runs; the SQL is purely a persistence shim.
UPDATE llm_provider
SET
    name = COALESCE(@name, name),
    base_url = COALESCE(@base_url, base_url),
    api_key_encrypted = COALESCE(@api_key_encrypted, api_key_encrypted),
    model_name = COALESCE(@model_name, model_name),
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
RETURNING *;

-- name: DeleteLLMProvider :exec
DELETE FROM llm_provider
WHERE id = $1 AND workspace_id = $2;
