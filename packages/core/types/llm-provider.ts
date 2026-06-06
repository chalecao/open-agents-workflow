/** LLM provider configuration — one workspace can have many, each bound to
 *  a single OpenAI-compatible HTTP endpoint (cloud provider or local
 *  server). The provider row is the secret-bearing source of truth; the
 *  server-side LLM execution worker reads it to make the upstream call
 *  when an agent bound to its paired runtime fires a task.
 *
 *  Wire shape mirrors `LLMProviderResponse` in
 *  `server/internal/handler/llm_provider.go`. The `api_key` is
 *  deliberately absent on the wire — the secretbox ciphertext never
 *  leaves the server. `has_api_key` is the only signal the UI gets that
 *  a key is set, so the form can render a masked placeholder without
 *  ever receiving the previous value back from a re-read.
 *  See CLAUDE.md → API Response Compatibility for the additive-only
 *  contract on these fields.
 */
export interface LLMProvider {
  id: string;
  workspace_id: string;
  name: string;
  base_url: string;
  has_api_key: boolean;
  model_name: string;
  /** Stable id of the auto-paired `openai-http` runtime. Exposed so the
   *  agent picker can deep-link "create agent on this provider" into the
   *  agent's runtime selection. */
  runtime_id: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

/** Body for POST /api/llm-providers. `api_key` is optional: local
 *  endpoints (e.g. local Ollama) frequently don't require auth. */
export interface CreateLLMProviderRequest {
  name: string;
  base_url: string;
  api_key?: string;
  model_name: string;
}

/** Body for PATCH /api/llm-providers/{id}. Every field is optional; the
 *  api_key contract is round-trip-safe:
 *   - field omitted / null → no change
 *   - api_key: ""          → no change (deliberate; UI round-trip-safe)
 *   - clear_api_key: true  → null the encrypted column
 *   - api_key: "..."       → re-seal and replace
 *  Mirrors the MUL-2600 secret-edit pattern. */
export interface UpdateLLMProviderRequest {
  name?: string;
  base_url?: string;
  api_key?: string;
  clear_api_key?: boolean;
  model_name?: string;
}
