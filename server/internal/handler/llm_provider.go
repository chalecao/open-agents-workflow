// Package handler — LLM provider endpoints. The LLM provider feature lets
// a workspace configure an OpenAI-compatible HTTP endpoint (cloud or local)
// and expose it as a runtime. The runtime row (provider='openai-http',
// runtime_mode='cloud') is what the agent picker surfaces; the llm_provider
// row is the secret-bearing source of truth that the server-side execution
// worker reads to make the HTTP call.
//
// Security:
//   - The API key is sealed with the same secretbox (AES-256-GCM) used for
//     Lark app_secret; key material is never logged or returned in plain
//     text on reads. The "has_api_key" boolean on the wire is the only
//     hint the UI gets that an api key is set — redaction mirrors how
//     `mcp_config` and the legacy `custom_env` are handled (MUL-2600).
//   - The base URL is validated server-side: must parse as an absolute
//     http(s) URL. We do not whitelist hosts — self-host operators may
//     point at internal endpoints (10.x, 192.168.x, *.local, …) that
//     resolve on the server's network.
//   - All write paths require workspace admin role. Reads are member-visible
//     so the agent picker can show "powered by LLM X" without exposing the
//     api key.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// openaiHTTPProvider is the provider value stamped on the auto-created
// runtime row. It must match what the server-side LLM execution worker
// (server/internal/llmexec) filters on, and the launch header returned
// to the UI.
const openaiHTTPProvider = "openai-http"

const (
	// maxLLMProviderNameLength mirrors AGENT_DESCRIPTION_MAX_LENGTH's
	// intent: a workspace-local handle that's discoverable in the
	// picker but never so long it overflows UI chips.
	maxLLMProviderNameLength = 64
	// maxLLMProviderModelLength is the wire-side cap on the model
	// string. Provider-supplied catalogs (e.g. ollama's
	// `llama3.1:70b-instruct-q4_K_M`) are well under 100 chars, so 200
	// is generous.
	maxLLMProviderModelLength = 200
)

// LLMProviderResponse is the wire shape for LLM provider reads and
// writes. `api_key_encrypted` is intentionally absent — the secretbox
// ciphertext never leaves the server. `has_api_key` is the only signal
// the UI gets that an api key is set, so the form can render a
// "•••••••" placeholder without ever receiving the previous value
// back from a re-read (MUL-2600-style redaction).
type LLMProviderResponse struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	HasAPIKey   bool      `json:"has_api_key"`
	ModelName   string    `json:"model_name"`
	RuntimeID   string    `json:"runtime_id"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateLLMProviderRequest is the body for POST /api/llm-providers.
// Empty API key is allowed for local endpoints that don't require auth.
type CreateLLMProviderRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model_name"`
}

// UpdateLLMProviderRequest is the body for PATCH. Every field is optional;
// empty string means "leave alone" except for `clear_api_key` which
// explicitly nulls the encrypted key. Treating empty api_key on a
// non-clear request as "no change" avoids the MUL-2600 foot-gun where a
// UI that round-trips a masked value accidentally clobbers a real secret.
type UpdateLLMProviderRequest struct {
	Name        *string `json:"name,omitempty"`
	BaseURL     *string `json:"base_url,omitempty"`
	APIKey      *string `json:"api_key,omitempty"`
	ClearAPIKey bool    `json:"clear_api_key,omitempty"`
	Model       *string `json:"model_name,omitempty"`
}

func llmProviderToResponse(p db.LlmProvider) LLMProviderResponse {
	return LLMProviderResponse{
		ID:          uuidToString(p.ID),
		WorkspaceID: uuidToString(p.WorkspaceID),
		Name:        p.Name,
		BaseURL:     p.BaseUrl,
		HasAPIKey:   len(p.ApiKeyEncrypted) > 0,
		ModelName:   p.ModelName,
		RuntimeID:   uuidToString(p.RuntimeID),
		CreatedBy:   uuidToString(p.CreatedBy),
		CreatedAt:   timestampToTime(p.CreatedAt),
		UpdatedAt:   timestampToTime(p.UpdatedAt),
	}
}

func timestampToTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// validateLLMProviderRequest runs the cross-field checks common to
// create + update. Returns the cleaned base URL and an error string
// suitable for 400 responses.
func validateLLMProviderRequest(name, baseURL, model string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}
	if len([]rune(name)) > maxLLMProviderNameLength {
		return "", fmt.Errorf("name must be %d characters or fewer", maxLLMProviderNameLength)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("model_name is required")
	}
	if len(model) > maxLLMProviderModelLength {
		return "", fmt.Errorf("model_name must be %d characters or fewer", maxLLMProviderModelLength)
	}
	cleaned, err := validateLLMBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	return cleaned, nil
}

// validateLLMBaseURL enforces an absolute http(s) URL with no embedded
// credentials. We do NOT whitelist hosts; self-host operators frequently
// point at 10.x / 192.168.x / *.local endpoints that only resolve on
// the server's network.
func validateLLMBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("base_url is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("base_url is not a valid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("base_url must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("base_url must include a host")
	}
	if u.User != nil {
		return "", errors.New("base_url must not contain credentials")
	}
	// Strip any trailing slash so concatenations like
	// baseURL + "/chat/completions" don't double up. Path components
	// are preserved as-is so providers that mount the API under a
	// sub-path (e.g. /openai/v1) keep working.
	trimmed = strings.TrimRight(trimmed, "/")
	return trimmed, nil
}

// sealLLMAPIKey returns the secretbox ciphertext for the given API key,
// or nil when key is empty. nil here means "no api key", not "encrypt
// the empty string" — the schema's nullable column stores a NULL when
// the operator chose a local endpoint that doesn't need auth.
func sealLLMAPIKey(box *secretbox.Box, key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	if box == nil {
		return nil, errors.New("api_key provided but server has no secretbox key configured (set MULTICA_LLM_SECRET_KEY)")
	}
	sealed, err := box.Seal([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("seal api key: %w", err)
	}
	return sealed, nil
}

// mustMarshal is a tiny helper for building the runtime.metadata json
// payload. map[string]any of primitives cannot fail to marshal, so the
// fallback is purely defensive.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// ListLLMProviders returns every LLM provider in the workspace, oldest
// first. Member-visible (the runtime picker needs to display them).
func (h *Handler) ListLLMProviders(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	rows, err := h.Queries.ListLLMProvidersByWorkspace(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list llm providers")
		return
	}
	resp := make([]LLMProviderResponse, len(rows))
	for i, p := range rows {
		resp[i] = llmProviderToResponse(p)
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetLLMProvider returns a single LLM provider by id. Member-visible.
func (h *Handler) GetLLMProvider(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "providerId"), "provider_id")
	if !ok {
		return
	}
	p, err := h.Queries.GetLLMProviderForWorkspace(r.Context(), db.GetLLMProviderForWorkspaceParams{
		ID:          id,
		WorkspaceID: parseUUID(workspaceID),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "llm provider not found")
		return
	}
	writeJSON(w, http.StatusOK, llmProviderToResponse(p))
}

// CreateLLMProvider inserts a new LLM provider + its paired runtime
// row in a single transaction so the (workspace_id, name) UNIQUE on
// llm_provider and the (workspace_id, daemon_id, provider) UNIQUE on
// agent_runtime are written atomically. Admin-only.
//
// We pre-mint the provider UUID in Go (rather than relying on the DB
// default) so we can stamp it into the runtime's daemon_id token up
// front: daemon_id is part of the agent_runtime UNIQUE constraint, so
// we cannot "UPDATE after the fact" — the row has to go in with its
// final daemon_id. That, in turn, lets us insert runtime + provider
// in a single pass with no delete-and-reupsert dance.
func (h *Handler) CreateLLMProvider(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if !h.requireWorkspaceAdmin(w, r, workspaceID, "admin required to create llm provider") {
		return
	}
	var req CreateLLMProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cleanedURL, err := validateLLMProviderRequest(req.Name, req.BaseURL, req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sealed, err := sealLLMAPIKey(h.LLMKeyBox, req.APIKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ownerID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID := parseUUID(workspaceID)
	ownerUUID := parseUUID(ownerID)

	// Pre-mint the provider UUID. The runtime's daemon_id will be
	// derived from this so the UNIQUE(workspace_id, daemon_id, provider)
	// distinguishes every LLM provider. We let the runtime mint its
	// own ID via UpsertAgentRuntime's default and reference it from
	// the provider row.
	preProviderID := uuid.New()
	daemonID := pgtype.Text{
		String: "llm-" + preProviderID.String(),
		Valid:  true,
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // committed below on success
	qtx := h.Queries.WithTx(tx)

	// Insert the runtime first so the FK from llm_provider is
	// satisfied. The (workspace_id, daemon_id, provider) UNIQUE
	// constraint will reject a collision with a previous run (e.g.
	// a half-deleted row from a failed transaction); we surface that
	// as 409 below.
	rt, err := qtx.UpsertAgentRuntime(r.Context(), db.UpsertAgentRuntimeParams{
		WorkspaceID: wsUUID,
		DaemonID:    daemonID,
		Name:        strings.TrimSpace(req.Name),
		RuntimeMode: "cloud",
		Provider:    openaiHTTPProvider,
		// online from creation: the worker polls only online runtimes,
		// so a freshly-created provider is immediately available for
		// tasks without waiting for a heartbeat.
		Status:     "online",
		DeviceInfo: "OpenAI-compatible LLM provider",
		Metadata:   mustMarshal(map[string]any{"llm_provider_id": preProviderID.String()}),
		OwnerID:    ownerUUID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "a runtime for this provider already exists")
			return
		}
		slog.Warn("create llm provider: runtime upsert failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", workspaceID)...)
		writeError(w, http.StatusInternalServerError, "failed to create llm provider runtime: "+err.Error())
		return
	}

	created, err := qtx.CreateLLMProvider(r.Context(), db.CreateLLMProviderParams{
		WorkspaceID:     wsUUID,
		Name:            strings.TrimSpace(req.Name),
		BaseUrl:         cleanedURL,
		ApiKeyEncrypted: sealed,
		ModelName:       strings.TrimSpace(req.Model),
		RuntimeID:       rt.ID,
		CreatedBy:       ownerUUID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if strings.Contains(pgErr.ConstraintName, "llm_provider_workspace_id_name") {
				writeError(w, http.StatusConflict, fmt.Sprintf("an llm provider named %q already exists in this workspace", req.Name))
				return
			}
		}
		slog.Warn("create llm provider failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", workspaceID)...)
		writeError(w, http.StatusInternalServerError, "failed to create llm provider: "+err.Error())
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Warn("create llm provider: commit failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", workspaceID)...)
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	slog.Info("llm provider created", append(logger.RequestAttrs(r),
		"workspace_id", workspaceID,
		"provider_id", uuidToString(created.ID),
		"runtime_id", uuidToString(created.RuntimeID),
	)...)
	writeJSON(w, http.StatusCreated, llmProviderToResponse(created))
}

// UpdateLLMProvider updates one or more fields on a LLM provider. API
// key updates follow MUL-2600-style semantics:
//   - field omitted / null → no change
//   - api_key: "" → no change (deliberately; round-trip-safe)
//   - clear_api_key: true → null the encrypted column
//   - api_key: "..."  → re-seal and replace
// Admin-only.
func (h *Handler) UpdateLLMProvider(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if !h.requireWorkspaceAdmin(w, r, workspaceID, "admin required to update llm provider") {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "providerId"), "provider_id")
	if !ok {
		return
	}
	var req UpdateLLMProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wsUUID := parseUUID(workspaceID)

	// Reject obvious conflicts. clear_api_key=true with a non-empty
	// api_key is ambiguous: a UI round-tripping both would silently
	// drop the new value. Surface it.
	if req.ClearAPIKey && req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		writeError(w, http.StatusBadRequest, "clear_api_key and api_key are mutually exclusive")
		return
	}

	params := db.UpdateLLMProviderParams{
		ID:          id,
		WorkspaceID: wsUUID,
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		if len([]rune(trimmed)) > maxLLMProviderNameLength {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("name must be %d characters or fewer", maxLLMProviderNameLength))
			return
		}
		params.Name = trimmed
	}
	if req.BaseURL != nil {
		cleaned, err := validateLLMBaseURL(*req.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		params.BaseUrl = cleaned
	}
	if req.Model != nil {
		trimmed := strings.TrimSpace(*req.Model)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "model_name must not be empty")
			return
		}
		if len(trimmed) > maxLLMProviderModelLength {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("model_name must be %d characters or fewer", maxLLMProviderModelLength))
			return
		}
		params.ModelName = trimmed
	}
	if req.ClearAPIKey {
		// nil in COALESCE means "leave alone"; to actually clear we
		// write an empty bytea. The COALESCE will treat
		// `''::bytea` as a non-NULL value and overwrite the
		// column. We use the empty bytea as the "set to empty"
		// sentinel; the handler-to-redaction layer treats
		// len()==0 as has_api_key=false.
		params.ApiKeyEncrypted = []byte{}
	} else if req.APIKey != nil {
		// Empty api_key here is treated as "no change" to keep
		// the round-trip-safe contract (a UI submitting
		// api_key="" must not wipe the stored secret).
		sealed, err := sealLLMAPIKey(h.LLMKeyBox, *req.APIKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if sealed != nil {
			params.ApiKeyEncrypted = sealed
		}
	}

	updated, err := h.Queries.UpdateLLMProvider(r.Context(), params)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			strings.Contains(pgErr.ConstraintName, "llm_provider_workspace_id_name") {
			writeError(w, http.StatusConflict, "an llm provider with that name already exists in this workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update llm provider: "+err.Error())
		return
	}
	slog.Info("llm provider updated", append(logger.RequestAttrs(r),
		"workspace_id", workspaceID,
		"provider_id", id.String(),
	)...)
	writeJSON(w, http.StatusOK, llmProviderToResponse(updated))
}

// DeleteLLMProvider removes the LLM provider and (via the FK ON DELETE
// CASCADE on runtime_id) its paired runtime. Any agents bound to that
// runtime will then fail canUseRuntimeForAgent on the next interaction.
// Admin-only.
func (h *Handler) DeleteLLMProvider(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if !h.requireWorkspaceAdmin(w, r, workspaceID, "admin required to delete llm provider") {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "providerId"), "provider_id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteLLMProvider(r.Context(), db.DeleteLLMProviderParams{
		ID:          id,
		WorkspaceID: parseUUID(workspaceID),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete llm provider")
		return
	}
	slog.Info("llm provider deleted", append(logger.RequestAttrs(r),
		"workspace_id", workspaceID,
		"provider_id", id.String(),
	)...)
	w.WriteHeader(http.StatusNoContent)
}

// requireWorkspaceAdmin gates the LLM provider write endpoints to
// workspace owner / admin. Reads are member-visible (the runtime picker
// needs to see "powered by LLM X"), writes are admin-only because
// creating a LLM provider effectively grants the workspace a new
// outbound HTTP channel that costs the operator money.
func (h *Handler) requireWorkspaceAdmin(w http.ResponseWriter, r *http.Request, workspaceID, msg string) bool {
	_, ok := h.requireWorkspaceRole(w, r, workspaceID, msg, "owner", "admin")
	return ok
}
