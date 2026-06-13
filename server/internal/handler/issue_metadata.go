package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Per-issue metadata is a small JSONB KV map agents use to record pipeline
// state (PR number, pipeline_status, waiting_on, ...). Three rules govern
// the V1 surface — they're enforced both in the handler and at the DB:
//
//   - keys match `^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$` (handler)
//   - at most 50 keys per issue (handler)
//   - values are primitive: string / number / bool (handler)
//   - JSONB column is an object and ≤ 8KB (DB CHECK; defense in depth)
//
// All mutations are single-key atomic. UpdateIssue does NOT touch metadata —
// any whole-blob overwrite would race with concurrent agent writes (see the
// design discussion on MUL-2017).
const (
	maxIssueMetadataKeys = 50
)

var issueMetadataKeyRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)

// SetIssueMetadataKeyRequest carries the JSON value to write under the key
// named in the URL. Value is a RawMessage so we can preserve numeric vs.
// string typing through to PostgreSQL — once decoded into `any`, JSON
// numbers all collapse to float64 and we'd lose integer fidelity.
type SetIssueMetadataKeyRequest struct {
	Value json.RawMessage `json:"value"`
}

func validateIssueMetadataKey(key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	if !issueMetadataKeyRE.MatchString(key) {
		return errors.New("key must match ^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$")
	}
	return nil
}

// validateIssueMetadataValue rejects anything other than a primitive JSON
// scalar. Null, arrays, and objects are not allowed — the V1 surface is
// flat KV. Removing a key uses DELETE, not a null value.
func validateIssueMetadataValue(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("value is required")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("value must be valid JSON: %w", err)
	}
	switch v.(type) {
	case string, bool, float64:
		return nil
	case nil:
		return errors.New("value cannot be null (use DELETE to remove a key)")
	default:
		return errors.New("value must be a primitive: string, number, or bool")
	}
}

// parseIssueMetadata decodes the JSONB bytes from db.Issue.Metadata into a
// Go map suitable for response serialization. Empty or unparseable blobs
// degrade to an empty map — the DB CHECK guarantees object shape, so this
// path is only hit on rows somehow predating the migration.
func parseIssueMetadata(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// parseIssueHandoffData mirrors parseIssueMetadata for the separate
// handoff_data JSONB column. Same shape (object, ≤ 8 KiB) and same
// fall-through-to-empty behaviour on legacy rows. Kept as a distinct
// helper so the two columns can diverge in future (e.g. handoff_data
// might allow nested objects that metadata forbids) without ripping up
// the call sites.
func parseIssueHandoffData(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// parseMetadataFilterParam reads the `metadata` query parameter (a JSON
// object) and returns it as the JSONB filter blob passed to ListIssues /
// CountIssues / ListOpenIssues. Empty input means "no filter" and returns
// a nil []byte, which the SQL layer interprets as "skip the @> check".
//
// Validates that the filter is itself a flat object of primitives, mirroring
// the constraints we apply at write time — querying for `{key: {nested}}`
// would never match since written values are primitive by construction.
func parseMetadataFilterParam(w http.ResponseWriter, raw string) ([]byte, bool) {
	if raw == "" {
		return nil, true
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		writeError(w, http.StatusBadRequest, "metadata filter must be a JSON object")
		return nil, false
	}
	for k, v := range parsed {
		if err := validateIssueMetadataKey(k); err != nil {
			writeError(w, http.StatusBadRequest, "metadata filter "+err.Error())
			return nil, false
		}
		switch v.(type) {
		case string, bool, float64:
			// ok
		default:
			writeError(w, http.StatusBadRequest, "metadata filter values must be primitives (string, number, bool)")
			return nil, false
		}
	}
	// Re-marshal so we send canonical JSON to PG (and not the raw, possibly
	// whitespace-padded user input).
	buf, err := json.Marshal(parsed)
	if err != nil {
		writeError(w, http.StatusBadRequest, "metadata filter is invalid")
		return nil, false
	}
	return buf, true
}

func (h *Handler) ListIssueMetadata(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metadata": parseIssueMetadata(issue.Metadata)})
}

func (h *Handler) SetIssueMetadataKey(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	key := chi.URLParam(r, "key")
	if err := validateIssueMetadataKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req SetIssueMetadataKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateIssueMetadataValue(req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	// Enforce the key-count cap in the handler. The DB only guards size,
	// and a clear 4xx for "too many keys" beats a CHECK violation that
	// happens to fire on the size cap once enough keys accumulate.
	existing := parseIssueMetadata(issue.Metadata)
	if _, present := existing[key]; !present && len(existing) >= maxIssueMetadataKeys {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("metadata cannot exceed %d keys", maxIssueMetadataKeys))
		return
	}

	updated, err := h.Queries.SetIssueMetadataKey(r.Context(), db.SetIssueMetadataKeyParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Key:         key,
		Value:       []byte(req.Value),
	})
	if err != nil {
		if isCheckViolation(err) {
			writeError(w, http.StatusBadRequest, "metadata exceeds the 8KB size limit")
			return
		}
		slog.Warn("SetIssueMetadataKey failed", append(logger.RequestAttrs(r), "error", err, "issue_id", issueID, "key", key)...)
		writeError(w, http.StatusInternalServerError, "failed to set metadata key")
		return
	}

	workspaceID := uuidToString(updated.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	metadata := parseIssueMetadata(updated.Metadata)
	h.publish(protocol.EventIssueMetadataChanged, workspaceID, actorType, actorID, map[string]any{
		"issue_id": uuidToString(updated.ID),
		"metadata": metadata,
	})
	writeJSON(w, http.StatusOK, map[string]any{"metadata": metadata})
}

func (h *Handler) DeleteIssueMetadataKey(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	key := chi.URLParam(r, "key")
	if err := validateIssueMetadataKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	updated, err := h.Queries.DeleteIssueMetadataKey(r.Context(), db.DeleteIssueMetadataKeyParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Key:         key,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "issue not found")
			return
		}
		slog.Warn("DeleteIssueMetadataKey failed", append(logger.RequestAttrs(r), "error", err, "issue_id", issueID, "key", key)...)
		writeError(w, http.StatusInternalServerError, "failed to delete metadata key")
		return
	}

	workspaceID := uuidToString(updated.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	metadata := parseIssueMetadata(updated.Metadata)
	h.publish(protocol.EventIssueMetadataChanged, workspaceID, actorType, actorID, map[string]any{
		"issue_id": uuidToString(updated.ID),
		"metadata": metadata,
	})
	writeJSON(w, http.StatusOK, map[string]any{"metadata": metadata})
}

// ── handoff_data ───────────────────────────────────────────────────────────
//
// handoff_data is a per-issue JSONB blob the autopilot handoff system
// reads to decide whether to fire a handoff rule and to interpolate into
// the @-mention comment body. It is intentionally separate from
// `metadata` so the two surfaces can evolve independently — metadata is a
// flat KV store restricted to primitives, while handoff_data accepts a
// nested object (e.g. {"summary": "...", "branch": "...", "urls": [...]})
// that the handoff template substitutes wholesale via {{handoff_data}}.
//
// The whole-blob shape is also deliberate: a source agent that finishes
// a task posts the full handoff payload in one shot. Single-key atomic
// writes (the metadata pattern) would force the agent to do N round-trips
// to assemble a structured payload, and the handoff rule engine needs
// the blob to be internally consistent when it evaluates. The trade-off
// is the same whole-blob race the metadata comment calls out — for
// handoff_data this is acceptable because only the source agent is
// expected to write it (the rule fires off the agent's next comment, so
// the agent is the only writer on the hot path).
//
// HandoffData constants: 50-key cap mirrors metadata, but since keys can
// be nested the cap is on the *top-level* keys to keep payloads small
// enough that the 8 KiB JSONB limit is unlikely to bind (avg key
// overhead is ~10 bytes).
const maxIssueHandoffDataTopKeys = 50

// SetIssueHandoffDataRequest accepts a free-form JSON object that becomes
// the new handoff_data value. The raw message is preserved through to the
// sqlc layer so the PG @> operator (used by the rule engine) sees the
// same byte shape the agent wrote — round-tripping through `any` would
// coerce integers to float64 etc.
type SetIssueHandoffDataRequest struct {
	HandoffData json.RawMessage `json:"handoff_data"`
}

// validateIssueHandoffData enforces:
//
//   - top-level value must be a JSON object (the column CHECK is the
//     last line of defense; we 400 first);
//   - at most 50 top-level keys, mirroring metadata;
//   - values may be any JSON shape (the handoff template renders the
//     whole object as JSON when interpolating, so nested arrays / objects
//     are the whole point of the field).
//
// Empty / null input is rejected — clients who want to clear the blob
// send an empty object `{}`.
func validateIssueHandoffData(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("handoff_data is required")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("handoff_data must be valid JSON: %w", err)
	}
	if v == nil {
		return errors.New("handoff_data cannot be null (use {} to clear)")
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("handoff_data must be a JSON object")
	}
	// Cap the top-level key count. The DB column size check (8 KiB) is
	// the second line of defense; rejecting pathological 10k-key objects
	// at the handler layer keeps the SQL path clean.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return fmt.Errorf("handoff_data is invalid: %w", err)
	}
	if len(asMap) > maxIssueHandoffDataTopKeys {
		return fmt.Errorf("handoff_data cannot exceed %d top-level keys", maxIssueHandoffDataTopKeys)
	}
	return nil
}

func (h *Handler) ListIssueHandoffData(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"handoff_data": parseIssueHandoffData(issue.HandoffData),
	})
}

func (h *Handler) SetIssueHandoffData(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")

	var req SetIssueHandoffDataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateIssueHandoffData(req.HandoffData); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	updated, err := h.Queries.SetIssueHandoffData(r.Context(), db.SetIssueHandoffDataParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
		HandoffData: []byte(req.HandoffData),
	})
	if err != nil {
		if isCheckViolation(err) {
			writeError(w, http.StatusBadRequest, "handoff_data exceeds the 8KB size limit or is not a JSON object")
			return
		}
		slog.Warn("SetIssueHandoffData failed", append(logger.RequestAttrs(r), "error", err, "issue_id", issueID)...)
		writeError(w, http.StatusInternalServerError, "failed to set handoff_data")
		return
	}

	workspaceID := uuidToString(updated.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	handoffData := parseIssueHandoffData(updated.HandoffData)
	// Re-use the metadata-changed event name with a sibling payload so
	// the existing WebSocket fan-out (issue:metadata:changed) wakes the
	// UI. Adding a dedicated event type would force every subscriber to
	// update its switch — the dispatch is the same (a key change on the
	// issue that may invalidate handoff rule eligibility).
	h.publish(protocol.EventIssueMetadataChanged, workspaceID, actorType, actorID, map[string]any{
		"issue_id":     uuidToString(updated.ID),
		"handoff_data": handoffData,
	})
	writeJSON(w, http.StatusOK, map[string]any{"handoff_data": handoffData})
}
