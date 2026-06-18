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

	"github.com/multica-ai/multica/server/internal/daemonws"
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

// TaskStarter is the subset of service.TaskService the worker
// needs to mark a dispatched task as running. Mirrors the daemon-side
// /tasks/{id}/start handler: transitions the row dispatched → running,
// captures analytics, and broadcasts protocol.EventTaskRunning so the
// workspace WS clients update their agent-activity indicator in real
// time (otherwise the UI lags by the React Query staleTime).
//
// Required because the LLM worker path bypasses the daemon HTTP
// flow — without an explicit start, the task stays in 'dispatched'
// and CompleteAgentTask's WHERE status='running' predicate never
// matches, which the caller (mis)interprets as "already finalized"
// and silently leaves the row stuck forever.
type TaskStarter interface {
	StartTask(ctx context.Context, taskID pgtype.UUID) (*db.AgentTaskQueue, error)
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
	starter   TaskStarter
	client    *OpenAIClient
	logger    *slog.Logger
	// listOpenaiHTTPRuntimesFn is the function Run uses to fetch
	// the openai-http runtime fleet each cycle. Defaults to the
	// real DB-backed implementation; tests override it to stub
	// the DB. The override lets a regression test hold the main
	// loop inside ExecuteOnce for arbitrarily long without
	// touching a database, while still letting the keep-alive
	// goroutine fire on its own cadence.
	listOpenaiHTTPRuntimesFn func(ctx context.Context) ([]db.AgentRuntime, error)
	// keepAliveFn is the function runKeepAliveLoop calls every
	// KeepAliveInterval. Defaults to the real keepAliveOnce;
	// tests override it to count ticks without touching the DB.
	// Decoupling the function pointer from the method is what
	// makes the "大模型离线了" regression testable: with both
	// functions stubbed, the test can keep the main loop busy
	// in ExecuteOnce and verify the keep-alive goroutine still
	// fires — exactly the property the original code violated.
	keepAliveFn func(ctx context.Context)
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
	// ToolsEnabled, when true, switches the worker from the
	// legacy single-turn plain-text path to the multi-turn
	// tool/function-calling path. The model gets a workdir
	// sandbox and a tool belt (read_file / write_file / list_dir
	// / run_shell / git_status / git_diff / git_commit) and is
	// expected to drive the task to completion in one or more
	// tool turns, ending with a final assistant message.
	//
	// Defaults to false so existing deployments keep their
	// pre-tool behaviour. cmd/server wires it from
	// MULTICA_LLM_TOOLS_ENABLED at boot; the per-provider
	// capability flag (worktree / commit / push) follows from
	// the model itself — providers that don't support
	// function calling will simply ignore the tools array and
	// fall back to text, which the worker also accepts as a
	// final answer.
	ToolsEnabled bool
	// ToolMaxTurns caps the number of LLM round-trips the
	// worker is willing to make per task. Defaults to 20 —
	// enough for a 5–10-file fix that interleaves reads,
	// writes, runs, and a couple of commits, low enough that
	// a runaway loop can't drain the LLM budget. A task that
	// hits the cap is failed with reason=tool_loop_budget.
	ToolMaxTurns int
	// RepocacheParent is the parent directory under which the
	// shared bare repo cache lives when the task is associated
	// with a project repo. The cache is per-workspace; multiple
	// tasks in the same workspace share one bare clone per repo
	// and only the per-task worktree is freshly allocated. Set
	// via MULTICA_LLM_REPOCACHE_PARENT. Leave empty to fall back
	// to the no-cache path (every task gets its own bare clone).
	RepocacheParent string
	// WorktreeParent is the parent directory under which each
	// task's per-run worktree is created. The worktree lives at
	//   <WorktreeParent>/<workspaceID>/<taskID>/
	// for repo-backed tasks, and at
	//   <WorktreeParent>/llmexec-XXX/
	// for tasks without a repo. Set via MULTICA_LLM_WORKTREE_PARENT.
	// Operators who want the worktrees on a dedicated SSD volume
	// (fast clone / checkout) override this env var.
	WorktreeParent string
	// ToolTimeout caps a single tool invocation (defaults to
	// 60s; run_shell can ask for less via its own arg). The
	// timeout is the *whole turn* — model round-trip + tool
	// dispatch — not just the tool, because the model can
	// otherwise stall and consume the queue.
	ToolTimeout time.Duration
	// DaemonHub is the shared daemon-WebSocket hub used by the
	// RemoteBackend to dispatch every file / shell / git op to
	// the daemon that owns a local_directory project. The
	// server's main.go wires it from daemonws.NewHub() at
	// boot. nil disables the local_directory path: tasks whose
	// project carries a local_directory resource fall back to
	// the empty-tempdir backend (the model gets a fresh
	// workdir instead of writing into the user's local
	// directory) and the activity log records a warning.
	DaemonHub *daemonws.Hub
	// RequestTimeout caps each individual RPC call dispatched
	// to a daemon. Defaults to 60 seconds. The whole turn
	// still has its own turn timeout in the worker; this is
	// the per-RPC ceiling, not the per-turn ceiling.
	RequestTimeout time.Duration
}

// NewWorker returns a worker. A nil box is permitted: in that case
// Run becomes a no-op and ExecuteOnce returns ErrDisabled. This lets
// the router wire a Worker unconditionally without a feature-flag
// branch at the call site.
//
// The supplied claimer, starter, and completer are normally the same
// *service.TaskService — the production wiring in cmd/server passes
// it three times. Splitting the interfaces in the constructor lets
// tests inject just one of them.
func NewWorker(queries *db.Queries, box *secretbox.Box, claimer TaskClaimer, starter TaskStarter, completer TaskCompleter, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Worker{
		queries:           queries,
		box:               box,
		claimer:           claimer,
		starter:           starter,
		completer:         completer,
		client:            NewOpenAIClient(),
		logger:            logger,
		PollInterval:      5 * time.Second,
		PerTaskTimeout:    2 * time.Minute,
		KeepAliveInterval: 60 * time.Second,
		KeepAliveTimeout:  10 * time.Second,
		ToolMaxTurns:      20,
		ToolTimeout:       60 * time.Second,
	}
	// Wire the indirection used by Run and runKeepAliveLoop.
	// Doing it in the constructor (rather than at every call
	// site) keeps the test override path single-step: tests set
	// the field on a freshly-constructed Worker, and the
	// function pointers above carry the override straight into
	// the goroutine without any extra plumbing.
	w.listOpenaiHTTPRuntimesFn = w.listOpenaiHTTPRuntimes
	w.keepAliveFn = w.keepAliveOnce
	return w
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
// The task-claim loop runs synchronously — a long task (multi-turn
// tool loop, big diff, remote-backend RPC) can keep ExecuteOnce
// blocked for minutes. To keep the runtime's last_seen_at fresh
// during that busy window, the keep-alive ticker runs in its own
// goroutine. Without that, an openai-http runtime with no daemon
// heartbeat would fall off the 150s stale window in
// cmd/server/runtime_sweeper.go mid-task, the sweeper would flip
// the row offline, and FailTasksForOfflineRuntimes would mark
// the in-flight task as failed with reason=runtime_offline —
// the "大模型离线了" the user sees. See the regression test in
// worker_test.go (TestRun_KeepAliveFiresDuringLongTask) for the
// pin.
func (w *Worker) Run(ctx context.Context) error {
	if !w.Enabled() {
		return ErrDisabled
	}
	w.logger.Info("llmexec worker starting")
	defer w.logger.Info("llmexec worker stopped")
	taskTicker := time.NewTicker(w.PollInterval)
	defer taskTicker.Stop()
	if w.KeepAliveInterval > 0 {
		// Independent goroutine: keep-alive must fire on its own
		// cadence even while the main for-loop is parked inside
		// ExecuteOnce waiting on a long LLM call. The DB writes
		// are idempotent (MarkAgentRuntimeOnline is a no-op on an
		// already-online row aside from refreshing last_seen_at)
		// so overlapping ticks — including one that lands while a
		// previous keepAliveOnce is still draining — are safe.
		go w.runKeepAliveLoop(ctx)
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
		}
	}
}

// runKeepAliveLoop is the dedicated keep-alive goroutine. It fires
// keepAliveFn (defaulting to keepAliveOnce) every KeepAliveInterval
// until ctx is cancelled, regardless of whether the main Run loop
// is currently busy with a task. That decoupling is the whole
// point — see the Run doc above and the regression test for the
// failure mode it fixes.
func (w *Worker) runKeepAliveLoop(ctx context.Context) {
	ticker := time.NewTicker(w.KeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.keepAliveFn(ctx)
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
	runtimes, err := w.listOpenaiHTTPRuntimesFn(ctx)
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
	// Transition dispatched → running before any upstream I/O. The
	// daemon-side flow does this via an explicit /tasks/{id}/start
	// HTTP call; the LLM worker has no daemon, so it has to do it
	// itself. Without this, the task stays in 'dispatched' and
	// CompleteAgentTask's WHERE status='running' predicate never
	// matches. CompleteTask's "already finalized" idempotency path
	// would then (mis)interpret the zero-row UPDATE as success and
	// return without running captureTaskCompleted, createAgentComment,
	// ReconcileAgentStatus, or broadcastTaskEvent — leaving the issue
	// stuck in "agent running" forever.
	started, err := w.starter.StartTask(ctx, task.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Task was cancelled or finalized by another path while
			// we were setting up. Nothing for us to do.
			w.logger.Info("llmexec: task no longer startable, skipping",
				"task_id", util.UUIDToString(task.ID),
			)
			return
		}
		w.failTask(ctx, task.ID, "could not transition task to running", "internal_error", err)
		return
	}
	// Refresh the local task copy so the rest of this function
	// (captureTaskCompleted, payload assembly) sees the now-running
	// row, with started_at populated.
	task = started
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
		// Defensive: ApiKeyEncrypted was non-empty but the plaintext
		// is the empty string. That means either the key was
		// originally stored as "" (UI bug) or the secretbox key in
		// MULTICA_LLM_KEY_BOX was rotated since the row was
		// inserted and GCM happened to validate an empty-body
		// ciphertext. Either way we MUST NOT silently send an
		// unauthenticated request to a provider that obviously has
		// a key configured — that's the exact failure mode that
		// produced MUL-XXXX "LLM call failed: 401 未提供令牌"
		// when runWithTools used to call w.client.Chat with a
		// hard-coded "" apiKey. Fail loudly so the operator can
		// re-encrypt the provider row.
		if apiKey == "" {
			w.failTask(ctx, task.ID, "llm api key decrypted to empty string; check MULTICA_LLM_KEY_BOX matches the key used when the provider row was created", "config_error", nil)
			return
		}
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
	// Branch: the tool path is a multi-turn loop that drives
	// the LLM through read → write → run → commit turns with a
	// workdir sandbox; the plain path is the legacy single-turn
	// Do() call. Both produce the same {output, provider, model,
	// runtime, ...} envelope, so downstream observers (chat,
	// activity, dashboard) don't need to special-case either
	// path — only the optional tool_audit fields are new.
	var result map[string]any
	if w.ToolsEnabled {
		result, err = w.runWithTools(ctx, rt, provider, task, apiKey, systemPrompt, userPrompt)
	} else {
		result, err = w.runPlainText(ctx, provider, apiKey, systemPrompt, userPrompt)
	}
	if err != nil {
		var upErr *ErrUpstream
		if errors.As(err, &upErr) {
			w.failTask(ctx, task.ID, fmt.Sprintf("llm call failed: %s", upErr.Error()), "upstream_error", err)
			return
		}
		w.failTask(ctx, task.ID, "llm call failed", "upstream_error", err)
		return
	}
	// Successful upstream call → refresh last_seen_at once on the
	// success path. The during-task liveness window is covered by
	// the keep-alive goroutine started in Run (every
	// KeepAliveInterval it pings /models and bumps last_seen_at on
	// the same runtime row), so an openai-http runtime with no
	// daemon heartbeat won't fall off the 150s stale window
	// mid-task. This end-of-task bump is a free signal that
	// doesn't wait up to KeepAliveInterval on the way out.
	w.bumpLiveness(ctx, rt.ID)
	// result envelope: same shape for both the plain and tool
	// paths (output, provider_id, model, runtime), so the
	// downstream chat/activity/dashboard consumers don't have to
	// special-case either. The tool path also carries an optional
	// tool_audit object with the changed-files list + commit SHAs,
	// which the activity log uses to render "5 files changed, 1
	// commit" summaries on autopilot runs.
	payload, err := json.Marshal(result)
	if err != nil {
		w.failTask(ctx, task.ID, "result marshal failed", "internal_error", err)
		return
	}
	if _, err := w.completer.CompleteTask(ctx, task.ID, payload, "", ""); err != nil {
		w.logger.Warn("llmexec complete failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
}

// runPlainText is the legacy single-turn path: one
// /chat/completions call with no tools, return the assistant
// text. Kept as a separate method so the tool path can swap in
// without disturbing the default behaviour.
func (w *Worker) runPlainText(ctx context.Context, provider db.LlmProvider, apiKey, systemPrompt, userPrompt string) (map[string]any, error) {
	callCtx, cancel := context.WithTimeout(ctx, w.PerTaskTimeout)
	defer cancel()
	output, err := w.client.Do(callCtx, provider.BaseUrl, provider.ModelName, apiKey, systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"output":      output,
		"provider_id": util.UUIDToString(provider.ID),
		"model":       provider.ModelName,
		"runtime":     "openai-http",
	}, nil
}

// runWithTools drives the LLM through a multi-turn tool loop:
//
//  1. Create a Worktree (sandboxed temp dir, optionally
//     initialised from a clone of the issue's project repo via
//     a shared per-workspace bare cache — same on-disk shape as
//     the daemon's CLI-runtime worktrees).
//  2. Build the initial messages = [system, user].
//  3. Loop up to ToolMaxTurns times: post the messages + tools to
//     the model, get back an assistant message. If the message has
//     tool_calls, dispatch each one through the registered tools
//     and append the result(s) as role:"tool" messages. Otherwise
//     the assistant message is the final answer — exit.
//  4. Return the final text + a tool_audit object describing the
//     changes the model made (so downstream consumers can show
//     "5 files changed, 1 commit" summaries).
//
// Errors are categorised so executeTask can route them:
//   - context.DeadlineExceeded / ToolMaxTurns hit → "tool_loop_budget"
//   - tool dispatch error that's not a tool ERROR: → "internal_error"
//   - upstream error from the LLM call → bubbles as *ErrUpstream
func (w *Worker) runWithTools(ctx context.Context, rt db.AgentRuntime, provider db.LlmProvider, task *db.AgentTaskQueue, apiKey, systemPrompt, userPrompt string) (map[string]any, error) {
	// Build the worktree assignment. The worker's task is
	// backend-agnostic; taskWorktreeAssignment picks the right
	// InitOptions based on the issue's project resources:
	//
	//   - local_directory resource → RemoteBackend rooted at
	//     the daemon's local_path. The model gets the user's
	//     directory as the workdir and every file op is
	//     dispatched to the daemon that owns the path.
	//   - github_repo resource → LocalBackend backed by the
	//     per-workspace bare cache + per-task worktree.
	//   - no project resources → LocalBackend as a fresh
	//     empty temp dir (chat / autopilot / quick-create).
	//
	// All three cases produce the same Worktree surface so
	// the tool loop below stays backend-agnostic.
	workspaceID := util.UUIDToString(rt.WorkspaceID)
	taskID := util.UUIDToString(task.ID)
	initOpts, err := w.taskWorktreeAssignment(ctx, task, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	wt := &Worktree{}
	if err := wt.Init(initOpts); err != nil {
		return nil, err
	}
	defer wt.Cleanup()
	tools := StandardTools()
	wireTools := ToolsToWire(tools)
	maxTurns := w.ToolMaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	turnTimeout := w.ToolTimeout
	if turnTimeout <= 0 {
		turnTimeout = 60 * time.Second
	}
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
	var lastContent string
	for turn := 0; turn < maxTurns; turn++ {
		turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
		assistant, err := w.client.Chat(turnCtx, provider.BaseUrl, provider.ModelName, apiKey, messages, wireTools)
		cancel()
		if err != nil {
			return nil, err
		}
		// Record the assistant's text even on tool turns — the
		// final answer is always whatever the assistant said
		// *after* the last tool result, but if the model chooses
		// to write a summary mid-loop we still want to capture it
		// as the user-visible output.
		if assistant.Content != "" {
			lastContent = assistant.Content
		}
		// Append the assistant message exactly as the model sent
		// it (including any tool_calls). The tool/result pairing
		// the wire protocol requires hinges on this — strip
		// tool_calls and the next role:"tool" message has no id
		// to attach to.
		messages = append(messages, assistant)
		if len(assistant.ToolCalls) == 0 {
			// No tool calls → that's the final answer.
			break
		}
		// Dispatch each tool call. Tool errors are surfaced to
		// the model as a role:"tool" message with a "ERROR: "
		// prefix so the model can self-correct; only dispatch
		// failures (no such tool, args parse error) bubble up.
		for _, call := range assistant.ToolCalls {
			out, derr := DispatchCall(ctx, wt, tools, call.Function.Name, call.Function.Arguments)
			if derr != nil {
				// Unknown tool / parse failure — this is a
				// model-level bug, surface verbatim.
				return nil, derr
			}
			messages = append(messages, ChatMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    out,
			})
		}
		if turn == maxTurns-1 {
			return nil, fmt.Errorf("llmexec: tool loop budget exhausted after %d turns", maxTurns)
		}
	}
	created, modified, deleted := wt.ChangedFiles()
	audit := map[string]any{
		"workdir":     wt.Root(),
		"created":     created,
		"modified":    modified,
		"deleted":     deleted,
		"commits":     wt.commits,
		"shell_calls": wt.shellCalls,
	}
	return map[string]any{
		"output":      lastContent,
		"provider_id": util.UUIDToString(provider.ID),
		"model":       provider.ModelName,
		"runtime":     "openai-http",
		"tool_audit":  audit,
	}, nil
}

// taskRepoURL looks up the github_repo resource for the task's
// issue (if any) and returns its URL. Returns "" with no error
// when the task has no issue, no project, or no github_repo
// resource — the workdir is then a clean empty tree. Errors are
// swallowed because a missing repo is not fatal: the model can
// still produce a text-only answer and the worker's tool path
// degrades to "write into an empty dir".
func (w *Worker) taskRepoURL(ctx context.Context, task *db.AgentTaskQueue) (string, error) {
	if !task.IssueID.Valid {
		return "", nil
	}
	issue, err := w.queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		return "", nil
	}
	if !issue.ProjectID.Valid {
		return "", nil
	}
	resources, err := w.queries.ListProjectResources(ctx, issue.ProjectID)
	if err != nil {
		return "", nil
	}
	for _, r := range resources {
		if r.ResourceType != "github_repo" {
			continue
		}
		var ref struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(r.ResourceRef, &ref); err != nil {
			continue
		}
		if ref.URL != "" {
			return ref.URL, nil
		}
	}
	return "", nil
}

// taskWorktreeAssignment decides which Worktree backend the worker
// should use for a given task and returns the InitOptions that
// drive Worktree.Init. The decision tree is:
//
//  1. The issue's project has a local_directory resource →
//     RemoteBackend rooted at the daemon's local_path. Every
//     file / shell / git op is dispatched as an RPC to the
//     daemon that owns the path.
//
//  2. The issue's project has a github_repo resource →
//     LocalBackend backed by the per-workspace bare cache +
//     per-task worktree. Same on-disk shape as the daemon's
//     CLI runtime.
//
//  3. Otherwise (chat / autopilot / quick-create with no
//     project) → LocalBackend as a fresh empty temp dir.
//
// The local_directory case requires three things wired into
// the worker: a non-nil DaemonHub (so RPCs have a transport),
// a DaemonID (for logging / auditing), and a RuntimeID for
// dispatch (the runtime that owns the local_path, NOT the
// openai-http runtime that claimed the task). When any of
// these is missing we fall back to the empty-tempdir path
// rather than failing the task — the model still gets a
// usable workdir, just not the user's real one. The
// fallback is logged at warn so an operator notices the
// missing wiring.
func (w *Worker) taskWorktreeAssignment(ctx context.Context, task *db.AgentTaskQueue, workspaceID, taskID string) (InitOptions, error) {
	opts := InitOptions{
		RepocacheParent: w.RepocacheParent,
		WorktreeParent:  w.WorktreeParent,
		WorkspaceID:     workspaceID,
		TaskID:          taskID,
		Logger:          w.logger,
	}
	// Look for a local_directory resource on the task's
	// project. If one is found AND the worker has a daemon
	// hub wired AND we can find a runtime owned by the
	// resource's daemon, route through RemoteBackend.
	if w.DaemonHub != nil {
		if loc := w.taskLocalDirectory(ctx, task); loc != nil {
			dispatchRuntimeID, err := w.findLocalDirectoryRuntime(ctx, workspaceID, loc.DaemonID)
			if err != nil {
				w.logger.Warn("llmexec: local_directory runtime lookup failed; falling back to empty workdir",
					"task_id", taskID,
					"local_path", loc.LocalPath,
					"daemon_id", loc.DaemonID,
					"error", err)
			} else if dispatchRuntimeID != "" {
				opts.LocalPath = loc.LocalPath
				opts.Hub = w.DaemonHub
				opts.DaemonID = loc.DaemonID
				opts.RuntimeID = dispatchRuntimeID
				opts.RequestTimeout = w.RequestTimeout
				return opts, nil
			} else {
				w.logger.Warn("llmexec: no online runtime for local_directory daemon; falling back to empty workdir",
					"task_id", taskID,
					"local_path", loc.LocalPath,
					"daemon_id", loc.DaemonID)
			}
		}
	}
	// Default: github_repo path (or no project at all).
	repoURL, _ := w.taskRepoURL(ctx, task)
	opts.RepoURL = repoURL
	return opts, nil
}

// localDirectoryResource is the parsed shape of a project
// resource of type "local_directory". The local_path is the
// absolute path on the daemon's filesystem; daemon_id
// identifies the daemon that owns the path (and therefore
// owns the WebSocket connection the LLM worker will dispatch
// RPCs to).
type localDirectoryResource struct {
	LocalPath string
	DaemonID  string
}

// taskLocalDirectory returns the first local_directory
// resource attached to the task's issue's project, or nil
// when the issue / project / resource is missing. Errors
// from the DB lookup are swallowed (same rationale as
// taskRepoURL — a missing project resource is not fatal;
// the worker falls back to the empty workdir).
func (w *Worker) taskLocalDirectory(ctx context.Context, task *db.AgentTaskQueue) *localDirectoryResource {
	if !task.IssueID.Valid {
		return nil
	}
	issue, err := w.queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		return nil
	}
	if !issue.ProjectID.Valid {
		return nil
	}
	resources, err := w.queries.ListProjectResources(ctx, issue.ProjectID)
	if err != nil {
		return nil
	}
	for _, r := range resources {
		if r.ResourceType != "local_directory" {
			continue
		}
		var ref struct {
			LocalPath string `json:"local_path"`
			DaemonID  string `json:"daemon_id"`
		}
		if err := json.Unmarshal(r.ResourceRef, &ref); err != nil {
			continue
		}
		if ref.LocalPath != "" && ref.DaemonID != "" {
			return &localDirectoryResource{LocalPath: ref.LocalPath, DaemonID: ref.DaemonID}
		}
	}
	return nil
}

// findLocalDirectoryRuntime picks an online runtime_id owned
// by daemonID in workspaceID. The runtime_id is the dispatch
// key the Hub uses to send RPCs to the daemon; the runtime
// itself can be any provider (claude-code, codex, …) because
// the RPC handler table is daemon-wide — it doesn't care
// which provider the runtime row advertises.
//
// Returns the empty string when no online runtime is found
// (the caller logs and falls back to the empty workdir).
// Returns an error only when the DB query itself fails.
func (w *Worker) findLocalDirectoryRuntime(ctx context.Context, workspaceID, daemonID string) (string, error) {
	wid := pgtype.UUID{}
	if err := wid.Scan(workspaceID); err != nil {
		return "", fmt.Errorf("parse workspace id: %w", err)
	}
	rows, err := w.queries.ListOnlineRuntimesByDaemonID(ctx, db.ListOnlineRuntimesByDaemonIDParams{
		WorkspaceID: wid,
		Lower:       daemonID,
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return util.UUIDToString(rows[0].ID), nil
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
