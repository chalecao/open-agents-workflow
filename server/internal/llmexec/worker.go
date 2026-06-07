package llmexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// openaiHTTPProvider is duplicated from internal/handler to avoid an
// import cycle (handler imports llmexec through Worker in production
// wiring). Keep the value in sync with
// internal/handler.openaiHTTPProvider.
const openaiHTTPProvider = "openai-http"

// TaskCompleter is the subset of service.TaskService the worker
// needs to report a finished task. Defined as an interface so tests
// can stub it without spinning up the full TaskService.
type TaskCompleter interface {
	CompleteTask(ctx context.Context, taskID pgtype.UUID, result []byte, sessionID, workDir string) (*db.AgentTaskQueue, error)
	FailTask(ctx context.Context, taskID pgtype.UUID, errMsg, sessionID, workDir, failureReason string) (*db.AgentTaskQueue, error)
}

// TaskClaimer is the subset of service.TaskService the worker needs
// to pull a task. The runtimeID is the openai-http runtime the LLM
// provider is bound to; ClaimTaskForRuntime returns nil when no task
// is pending, which the worker treats as "sleep, nothing to do".
//
// Splitting this from TaskCompleter (rather than merging both into
// one TaskServiceClient interface) lets callers wire a stub completer
// in tests that can drive ExecuteTask without standing up a claimer.
type TaskClaimer interface {
	ClaimTaskForRuntime(ctx context.Context, runtimeID pgtype.UUID) (*db.AgentTaskQueue, error)
}

// DBTX is the minimal DB surface the worker uses. *db.Queries
// implements it; pgxpool.Pool also implements it via DBTX so tests
// can pass an in-process pool directly.
type DBTX interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (sql.Result, error)
}

// Worker polls openai-http runtimes for queued tasks and executes
// them against the configured LLM endpoint. One Worker per process
// is enough — it picks runtimes up dynamically so a single instance
// services every workspace's LLM providers.
type Worker struct {
	queries   *db.Queries
	box       *secretbox.Box
	completer TaskCompleter
	claimer   TaskClaimer
	client    *OpenAIClient
	logger    *slog.Logger
	// PollInterval is the wait between two poll cycles when no task
	// is pending. Defaults to 5s. Set lower in tests to drive the
	// loop deterministically.
	PollInterval time.Duration
	// PerTaskTimeout caps a single upstream LLM call. Defaults to
	// 2 minutes. The task itself retains its own deadline; this is
	// the LLM call's per-attempt budget.
	PerTaskTimeout time.Duration
	// KeepAliveInterval is the cadence at which the worker pings
	// every openai-http runtime's /models endpoint and refreshes
	// its last_seen_at (and flips the status field). Defaults to
	// 60s, well under staleThresholdSeconds (150s) so a healthy
	// provider never goes offline just because no task ran through
	// it. Set to 0 to disable keep-alive (the success-path bump in
	// executeTask still runs).
	KeepAliveInterval time.Duration
	// KeepAliveTimeout caps a single ping. Defaults to 10s.
	KeepAliveTimeout time.Duration
}

// NewWorker returns a worker. A nil box is permitted: in that case
// Run becomes a no-op and ExecuteOnce returns ErrDisabled. This lets
// the router wire a Worker unconditionally without a feature-flag
// branch at the call site.
//
// The supplied claimer and completer are normally the same
// *service.TaskService — the production wiring in cmd/server passes
// it twice. Splitting the interfaces in the constructor lets tests
// inject just one of them.
func NewWorker(queries *db.Queries, box *secretbox.Box, claimer TaskClaimer, completer TaskCompleter, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		queries:           queries,
		box:               box,
		claimer:           claimer,
		completer:         completer,
		client:            NewOpenAIClient(),
		logger:            logger,
		PollInterval:      5 * time.Second,
		PerTaskTimeout:    2 * time.Minute,
		KeepAliveInterval: 60 * time.Second,
		KeepAliveTimeout:  10 * time.Second,
	}
}

// Enabled reports whether the worker has a usable secretbox. A
// caller can check this before spawning the Run loop to avoid
// noisy "feature off" log lines.
func (w *Worker) Enabled() bool {
	return w != nil && w.box != nil
}

// Run drives the worker until ctx is cancelled. Each cycle:
//  1. List every online openai-http runtime in the workspace
//     (we don't have a per-workspace worker — one global worker).
//  2. For each runtime, call ClaimTaskForRuntime.
//  3. If a task is claimed, call ExecuteTask and let it report back
//     via completer.
//  4. Sleep PollInterval between cycles (also clamped by ctx).
//
// In parallel with the task-claim loop, a separate keep-alive
// ticker pings every openai-http runtime's /models endpoint every
// KeepAliveInterval and refreshes last_seen_at (or flips the row
// offline if the ping fails). This is what keeps the runtime's
// UI status honest: openai-http runtimes have no daemon-side
// heartbeat, so without this pass the 150s sweeper in
// cmd/server/runtime_sweeper.go would flip every healthy provider
// to offline on idle.
func (w *Worker) Run(ctx context.Context) error {
	if !w.Enabled() {
		return ErrDisabled
	}
	w.logger.Info("llmexec worker starting")
	defer w.logger.Info("llmexec worker stopped")
	taskTicker := time.NewTicker(w.PollInterval)
	defer taskTicker.Stop()
	var keepAliveCh <-chan time.Time
	if w.KeepAliveInterval > 0 {
		keepAliveTicker := time.NewTicker(w.KeepAliveInterval)
		defer keepAliveTicker.Stop()
		keepAliveCh = keepAliveTicker.C
	}
	for {
		// Drain immediately on entry so a process restart that
		// happened mid-cycle does not have to wait a full
		// PollInterval before picking up the first task.
		if err := w.ExecuteOnce(ctx); err != nil && !errors.Is(err, ErrDisabled) {
			w.logger.Warn("llmexec cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-taskTicker.C:
		case <-keepAliveCh:
			w.keepAliveOnce(ctx)
		}
	}
}

// ExecuteOnce runs a single poll cycle. Returns ErrDisabled when
// the worker is not configured. Any other non-nil error is a
// transient failure that the next cycle will retry; the worker
// does not bail on a single bad cycle.
func (w *Worker) ExecuteOnce(ctx context.Context) error {
	if !w.Enabled() {
		return ErrDisabled
	}
	runtimes, err := w.listOpenaiHTTPRuntimes(ctx)
	if err != nil {
		return fmt.Errorf("list openai-http runtimes: %w", err)
	}
	if len(runtimes) == 0 {
		return nil
	}
	for _, rt := range runtimes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only claim from runtimes that are still online. A
		// runtime that has been flipped to offline (e.g. the
		// workspace disabled the provider) must not pull more
		// work.
		if rt.Status != "online" {
			continue
		}
		task, err := w.claimer.ClaimTaskForRuntime(ctx, rt.ID)
		if err != nil {
			w.logger.Warn("llmexec claim failed",
				"runtime_id", util.UUIDToString(rt.ID),
				"error", err)
			continue
		}
		if task == nil {
			continue
		}
		w.executeTask(ctx, rt, task)
	}
	return nil
}

// executeTask builds the prompt, makes the upstream call, and
// reports the result. Errors are funnelled to completer.FailTask
// so the task never silently hangs in `running`.
func (w *Worker) executeTask(ctx context.Context, rt db.AgentRuntime, task *db.AgentTaskQueue) {
	provider, err := w.queries.GetLLMProviderByRuntime(ctx, rt.ID)
	if err != nil {
		w.failTask(ctx, task.ID, "llm provider not found for runtime", "config_error", err)
		return
	}
	apiKey := ""
	if len(provider.ApiKeyEncrypted) > 0 {
		plain, err := w.box.Open(provider.ApiKeyEncrypted)
		if err != nil {
			w.failTask(ctx, task.ID, "llm api key could not be decrypted", "config_error", err)
			return
		}
		apiKey = string(plain)
	}
	agent, err := w.queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		w.failTask(ctx, task.ID, "agent not found", "config_error", err)
		return
	}
	systemPrompt, userPrompt, err := buildPrompts(ctx, w.queries, agent, task)
	if err != nil {
		w.failTask(ctx, task.ID, "could not build prompt", "config_error", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, w.PerTaskTimeout)
	defer cancel()
	output, err := w.client.Do(callCtx, provider.BaseUrl, provider.ModelName, apiKey, systemPrompt, userPrompt)
	if err != nil {
		var upErr *ErrUpstream
		if errors.As(err, &upErr) {
			w.failTask(ctx, task.ID, fmt.Sprintf("llm call failed: %s", upErr.Error()), "upstream_error", err)
			return
		}
		w.failTask(ctx, task.ID, "llm call failed", "upstream_error", err)
		return
	}
	// Successful upstream call → refresh last_seen_at. The 150s
	// stale-sweeper in cmd/server/runtime_sweeper.go would otherwise
	// flip a healthy openai-http runtime offline on the first idle
	// window, since no daemon is heartbeating for it. The keep-alive
	// pass below covers the idle case; this success-path bump is a
	// free signal that doesn't wait up to KeepAliveInterval.
	w.bumpLiveness(ctx, rt.ID)
	// Wrap the LLM output in a tiny envelope so the consumer of the
	// task result can distinguish a successful LLM call from a
	// claimed-but-empty output. The daemon writes its result as a
	// markdown blob; the result chat sees a `{ "output": "..." }`
	// envelope with the LLM's text. Follow-up work could surface
	// usage here.
	result := map[string]any{
		"output":      output,
		"provider_id": util.UUIDToString(provider.ID),
		"model":       provider.ModelName,
		"runtime":     "openai-http",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		w.failTask(ctx, task.ID, "result marshal failed", "internal_error", err)
		return
	}
	if _, err := w.completer.CompleteTask(ctx, task.ID, payload, "", ""); err != nil {
		w.logger.Warn("llmexec complete failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
}

func (w *Worker) failTask(ctx context.Context, taskID pgtype.UUID, msg, reason string, cause error) {
	w.logger.Warn("llmexec failing task",
		"task_id", util.UUIDToString(taskID),
		"reason", reason,
		"error", cause,
	)
	if _, err := w.completer.FailTask(ctx, taskID, msg, "", "", reason); err != nil {
		w.logger.Warn("llmexec fail-task call failed",
			"task_id", util.UUIDToString(taskID),
			"error", err,
		)
	}
}

// listOpenaiHTTPRuntimes returns every online openai-http runtime.
// The query is hand-written (not via sqlc) because the slice
// returned is consumed inline and the schema is a join we don't
// have a generated type for.
func (w *Worker) listOpenaiHTTPRuntimes(ctx context.Context) ([]db.AgentRuntime, error) {
	return w.queries.ListOnlineRuntimesByProvider(ctx, openaiHTTPProvider)
}

// bumpLiveness flips the runtime's status to 'online' and refreshes
// last_seen_at. Safe to call from any path that has just successfully
// reached the upstream endpoint. Errors are logged but not returned
// — a failed bump just means the next keep-alive tick (or the
// 150s sweeper, in the worst case) will re-establish liveness.
func (w *Worker) bumpLiveness(ctx context.Context, runtimeID pgtype.UUID) {
	if _, err := w.queries.MarkAgentRuntimeOnline(ctx, runtimeID); err != nil {
		w.logger.Warn("llmexec: bump liveness failed",
			"runtime_id", util.UUIDToString(runtimeID),
			"error", err,
		)
	}
}

// keepAliveOnce pings every openai-http runtime's /models endpoint
// and refreshes its last_seen_at. Runtimes that fail the ping are
// flipped offline (only if currently online, so we don't churn rows
// that are already in the right state). Iterating over ALL
// openai-http runtimes — not just the online ones — means a runtime
// whose URL was fixed (e.g. after a typo correction in the
// LLM provider config) recovers online on the next tick without
// requiring a provider re-save.
//
// Bounded by the configured KeepAliveTimeout per runtime; the
// whole pass is bounded by ctx (so a server shutdown aborts
// promptly).
func (w *Worker) keepAliveOnce(ctx context.Context) {
	if !w.Enabled() {
		return
	}
	rows, err := w.queries.ListRuntimesByProvider(ctx, openaiHTTPProvider)
	if err != nil {
		w.logger.Warn("llmexec keep-alive: list failed", "error", err)
		return
	}
	for _, rt := range rows {
		if ctx.Err() != nil {
			return
		}
		provider, err := w.queries.GetLLMProviderByRuntime(ctx, rt.ID)
		if err != nil {
			// Orphaned runtime (provider row gone but FK cascade
			// hasn't run yet, or hand-crafted test data). Flip
			// offline so the UI doesn't keep advertising a
			// non-functional runtime.
			if rt.Status == "online" {
				if err := w.queries.SetAgentRuntimeOffline(ctx, rt.ID); err != nil {
					w.logger.Warn("llmexec keep-alive: set offline failed",
						"runtime_id", util.UUIDToString(rt.ID),
						"error", err,
					)
				}
			}
			continue
		}
		apiKey := ""
		if len(provider.ApiKeyEncrypted) > 0 {
			plain, err := w.box.Open(provider.ApiKeyEncrypted)
			if err != nil {
				w.logger.Warn("llmexec keep-alive: decrypt api key failed",
					"runtime_id", util.UUIDToString(rt.ID),
					"error", err,
				)
				continue
			}
			apiKey = string(plain)
		}
		timeout := w.KeepAliveTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		pingCtx, cancel := context.WithTimeout(ctx, timeout)
		pingErr := w.client.Ping(pingCtx, provider.BaseUrl, apiKey)
		cancel()
		if pingErr != nil {
			if rt.Status == "online" {
				if err := w.queries.SetAgentRuntimeOffline(ctx, rt.ID); err != nil {
					w.logger.Warn("llmexec keep-alive: set offline failed",
						"runtime_id", util.UUIDToString(rt.ID),
						"error", err,
					)
				} else {
					w.logger.Info("llmexec keep-alive: runtime went offline",
						"runtime_id", util.UUIDToString(rt.ID),
						"ping_error", pingErr,
					)
				}
			}
			continue
		}
		// Ping succeeded → mark online. MarkAgentRuntimeOnline
		// is a no-op write when the row is already online, but
		// it's the only path that also refreshes last_seen_at.
		if _, err := w.queries.MarkAgentRuntimeOnline(ctx, rt.ID); err != nil {
			w.logger.Warn("llmexec keep-alive: mark online failed",
				"runtime_id", util.UUIDToString(rt.ID),
				"error", err,
			)
		}
	}
}
