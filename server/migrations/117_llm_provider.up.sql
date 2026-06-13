-- Workspace-level LLM provider configuration. Stores the base URL, model
-- name, and (optional) encrypted API key for OpenAI-compatible endpoints
-- that the workspace wants to expose as agent runtimes. Self-hosted
-- deployments can use this to plug a local LLM (Ollama, vLLM, llama.cpp's
-- OpenAI shim, ...) or a private cloud model (DeepSeek, OpenRouter, a
-- corporate Anthropic-compatible gateway, ...) into the agent flow
-- without having to install any of the third-party CLIs the daemon
-- normally spawns.
--
-- Each row is paired 1:1 with an `agent_runtime` row (provider =
-- 'openai-http', runtime_mode = 'cloud') at create time. The runtime
-- row is what the agent picker actually shows; the llm_provider row is
-- the secret-bearing source of truth that the server-side LLM execution
-- worker reads to make the HTTP call.
--
-- api_key_encrypted is the secretbox-sealed form (nonce || ciphertext ||
-- tag). NULL is allowed for local endpoints that don't require auth.
-- base_url must be a valid http(s) URL; the check is enforced at the
-- handler layer with `url.ParseRequestURI` + scheme allowlist.

CREATE TABLE llm_provider (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    base_url TEXT NOT NULL,
    api_key_encrypted BYTEA,
    model_name TEXT NOT NULL,
    -- runtime_id is the auto-created openai-http runtime paired with this
    -- provider. CASCADE on the runtime row would also drop the provider,
    -- which is the right semantic for "delete the LLM = drop the agents
    -- that depend on it" — those agents are not useful without their
    -- LLM config.
    runtime_id UUID NOT NULL REFERENCES agent_runtime(id) ON DELETE CASCADE,
    created_by UUID NOT NULL REFERENCES "user"(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE INDEX idx_llm_provider_workspace ON llm_provider(workspace_id);

-- updated_at is set by the handler on every update (mirrors how the
-- existing tables do it — there's no shared set_updated_at() function
-- in this schema; the runtime/workspace/etc. handlers all do
-- `SET updated_at = now()` themselves).
