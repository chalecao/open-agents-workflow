package llmexec

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// scriptedTurn is one scripted assistant response. A "tool" turn
// returns tool_calls that the worker should dispatch; a "final"
// turn returns plain content and tells the loop to exit.
type scriptedTurn struct {
	toolCalls []scriptedCall // non-empty → tool turn
	content   string         // non-empty → final answer
}

type scriptedCall struct {
	name      string
	arguments string
	// id is the wire-format id we'll attach to the call. Tests
	// that need to assert on it can read it back from the
	// recorded requests; the loop itself doesn't depend on it.
	id string
}

// TestRunWithTools_EndToEnd stands up a fake OpenAI-compatible
// server, points the worker at it, and runs the multi-turn loop
// against a scripted LLM that:
//
//	turn 1: write_file hello.txt = "hi"
//	turn 2: git_commit "add hello"
//	turn 3: final answer "done"
//
// The script is what the real task system feeds to the worker —
// the LLM picks a tool, the worker dispatches, the LLM picks
// the next tool, the worker dispatches again, etc. The test
// asserts:
//
//   - 3 round-trips happened (no extra calls, no premature exit)
//   - the workdir contains hello.txt with the written content
//   - the worker's audit map records the commit
//   - the assistant's final text bubbles up as the result output
//
// The mock server also asserts on the wire shape it received:
// the first call must include the tools array, every call after
// the first must echo the prior tool message back, etc. A
// regression in the loop's message bookkeeping would flip one
// of these checks.
func TestRunWithTools_EndToEnd(t *testing.T) {
	// Scripted LLM turns. turn-1 emits a tool call; turn-2
	// emits the commit; turn-3 returns the final answer.
	turns := []scriptedTurn{
		{
			toolCalls: []scriptedCall{
				{
					name:      "write_file",
					arguments: `{"path":"hello.txt","content":"hi from script"}`,
					id:        "call_write",
				},
			},
		},
		{
			toolCalls: []scriptedCall{
				{
					name:      "git_commit",
					arguments: `{"message":"add hello"}`,
					id:        "call_commit",
				},
			},
		},
		{
			content: "all done, committed.",
		},
	}
	var turnIdx int32
	var callCount int32
	var firstCallHadTools bool
	// lastToolID is the id we expect on the next role:"tool"
	// message, set when a turn emits a tool_call. This is how
	// we lock the tool_call_id pairing the wire protocol
	// requires.
	var lastToolID string
	// pendingToolID is set when the mock emits a tool call and
	// reset when the next request arrives with the matching
	// role:"tool" message. A second turn that emits a different
	// id without a matching tool message is a wire bug.
	pendingToolID := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		buf := make([]byte, 32<<10)
		n, _ := r.Body.Read(buf)
		_ = json.Unmarshal(buf[:n], &req)
		atomic.AddInt32(&callCount, 1)
		if atomic.LoadInt32(&callCount) == 1 {
			if len(req.Tools) == 0 {
				t.Errorf("first call must include tools array")
			} else {
				firstCallHadTools = true
			}
		}
		// Verify the conversation shape: messages should always
		// be [system, user, ...]. The tool/assistant alternation
		// is implicit in how we appended — assert that the last
		// message, when a pendingToolID is set, is a role:"tool"
		// with that id.
		if pendingToolID != "" && len(req.Messages) > 0 {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" {
				t.Errorf("expected last message role=tool, got %q", last.Role)
			}
			if last.ToolCallID != pendingToolID {
				t.Errorf("expected last tool_call_id=%q, got %q", pendingToolID, last.ToolCallID)
			}
		}
		// Consume the scripted turn.
		idx := int(atomic.LoadInt32(&turnIdx))
		if idx >= len(turns) {
			t.Errorf("LLM called too many times: %d, script has %d", idx, len(turns))
			w.Write([]byte(`{"choices":[{"message":{"content":"out of script"}}]}`))
			return
		}
		turn := turns[idx]
		atomic.AddInt32(&turnIdx, 1)
		if len(turn.toolCalls) > 0 {
			calls := make([]ToolCall, 0, len(turn.toolCalls))
			for _, c := range turn.toolCalls {
				tc := ToolCall{ID: c.id, Type: "function"}
				tc.Function.Name = c.name
				tc.Function.Arguments = c.arguments
				calls = append(calls, tc)
				lastToolID = c.id
			}
			pendingToolID = lastToolID
			resp := map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{
						"role":       "assistant",
						"content":    "",
						"tool_calls": calls,
					}},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Final-answer turn.
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{
					"role":    "assistant",
					"content": turn.content,
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Build a worker with ToolsEnabled=true and a short
	// timeout. We don't go through the real claim / start /
	// complete plumbing — the test calls runWithTools directly
	// so it can assert on the returned envelope without
	// standing up a database.
	w := &Worker{
		client:       NewOpenAIClient(),
		logger:       discardLogger(),
		ToolMaxTurns: 10,
		ToolTimeout:  5 * time.Second,
	}
	provider := db.LlmProvider{
		BaseUrl:   srv.URL,
		ModelName: "scripted",
	}
	// Construct a task with no issue, no project — the worktree
	// is empty (no repo clone). The runtime carries a workspace
	// id, but with RepoURL empty the worker skips the bare
	// cache and falls back to the temp-dir path.
	task := &db.AgentTaskQueue{}
	rt := db.AgentRuntime{}
	res, err := w.runWithTools(context.Background(), rt, provider, task, "test-api-key", "you are scripted", "do the thing")
	if err != nil {
		t.Fatalf("runWithTools: %v", err)
	}
	// Assertions on the result envelope.
	if !firstCallHadTools {
		t.Error("first call to LLM did not include tools")
	}
	if got := int(atomic.LoadInt32(&callCount)); got != len(turns) {
		t.Errorf("expected %d LLM round-trips, got %d", len(turns), got)
	}
	if out, _ := res["output"].(string); !strings.Contains(out, "all done") {
		t.Errorf("output: got %q, want contains 'all done'", out)
	}
	if res["runtime"] != "openai-http" {
		t.Errorf("runtime: got %v", res["runtime"])
	}
	audit, ok := res["tool_audit"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_audit map, got %T", res["tool_audit"])
	}
	created, _ := audit["created"].([]string)
	modified, _ := audit["modified"].([]string)
	if len(created) != 1 || created[0] != "hello.txt" {
		t.Errorf("created: got %v, want [hello.txt]", created)
	}
	// The model didn't touch any pre-existing file, so modified
	// should be empty. (write_file puts new.txt in created, the
	// commit doesn't add to modified.)
	if len(modified) != 0 {
		t.Errorf("modified: got %v, want []", modified)
	}
	commits, _ := audit["commits"].([]CommitRecord)
	if len(commits) != 1 {
		t.Fatalf("commits: got %d, want 1", len(commits))
	}
	if len(commits[0].SHA) < 7 {
		t.Errorf("commit SHA too short: %q", commits[0].SHA)
	}
	if !strings.Contains(commits[0].Message, "add hello") {
		t.Errorf("commit message: got %q", commits[0].Message)
	}
}

// TestRunWithTools_BudgetExhausted guards the safety cap: if the
// model keeps emitting tool calls, the loop eventually fails
// rather than spinning forever. The script is "ask the model to
// write a file 30 times" and the cap is 5 — the test asserts the
// error mentions "tool loop budget".
func TestRunWithTools_BudgetExhausted(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{
						{"id": "c1", "type": "function", "function": map[string]any{
							"name":      "write_file",
							"arguments": `{"path":"loop.txt","content":"x"}`,
						}},
					},
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	w := &Worker{
		client:       NewOpenAIClient(),
		logger:       discardLogger(),
		ToolMaxTurns: 3,
		ToolTimeout:  5 * time.Second,
	}
	provider := db.LlmProvider{BaseUrl: srv.URL, ModelName: "spin"}
	_, err := w.runWithTools(context.Background(), db.AgentRuntime{}, provider, &db.AgentTaskQueue{}, "test-api-key", "sys", "go")
	if err == nil {
		t.Fatal("expected budget error")
	}
	if !strings.Contains(err.Error(), "tool loop budget") {
		t.Errorf("err: got %v, want contains 'tool loop budget'", err)
	}
	// Should have made at most 3 round-trips.
	if got := int(atomic.LoadInt32(&callCount)); got > 3 {
		t.Errorf("LLM called %d times, want <= 3", got)
	}
}

// discardLogger returns a *slog.Logger that swallows every
// record. The worker's logger is only used for non-fatal
// warnings (liveness bumps, completion failures, …) — we don't
// want those polluting `go test -v` output, and we don't want
// to import a per-test slog handler just for that. io.Discard
// is the cheapest sink.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestTaskWorktreeAssignment_NoProject_FallsBackToLocal is the
// simplest possible routing case: a task with no project
// resources (chat, autopilot, quick-create with no repo) must
// produce InitOptions that resolve to the empty-tempdir
// LocalBackend. This is the common path for the legacy
// openai-http runtime that never had a project; if the
// assignment logic ever started demanding a Hub or LocalPath
// the LLM worker would start failing every chat task.
func TestTaskWorktreeAssignment_NoProject_FallsBackToLocal(t *testing.T) {
	w := &Worker{
		queries: nil, // taskRepoURL will skip the DB lookup on a zero-value task
		logger:  discardLogger(),
	}
	// Task with no issue / no project.
	task := &db.AgentTaskQueue{}
	opts, err := w.taskWorktreeAssignment(context.Background(), task, "ws-1", "task-1")
	if err != nil {
		t.Fatalf("taskWorktreeAssignment: %v", err)
	}
	if opts.Hub != nil {
		t.Errorf("Hub: got non-nil, want nil for no-project task")
	}
	if opts.LocalPath != "" {
		t.Errorf("LocalPath: got %q, want empty (no local_directory)", opts.LocalPath)
	}
	if opts.RepoURL != "" {
		t.Errorf("RepoURL: got %q, want empty (no github_repo)", opts.RepoURL)
	}
	if opts.WorkspaceID != "ws-1" || opts.TaskID != "task-1" {
		t.Errorf("scope: got (%q,%q), want (ws-1,task-1)", opts.WorkspaceID, opts.TaskID)
	}
}

// TestTaskWorktreeAssignment_DaemonHubRequiredForLocalDir
// pins the gating rule: a local_directory project only
// produces a RemoteBackend routing when the worker has a
// DaemonHub wired. Without the hub, the assignment must
// gracefully fall back to LocalBackend (no project
// resources) so the model still gets a usable workdir,
// rather than failing the task with a transport error.
func TestTaskWorktreeAssignment_DaemonHubRequiredForLocalDir(t *testing.T) {
	w := &Worker{
		// DaemonHub left nil: the gating is "hub must be
		// wired before we trust a local_directory
		// resource". Tests that exercise the actual
		// RemoteBackend construction go through the
		// RemoteBackend tests (remote_backend_test.go).
		DaemonHub: nil,
		logger:    discardLogger(),
	}
	// taskLocalDirectory returns nil when there's no
	// project (the test task is empty), so the
	// assignment skips the local_directory branch
	// entirely. We assert that the result is the
	// LocalBackend fallback.
	opts, err := w.taskWorktreeAssignment(context.Background(), &db.AgentTaskQueue{}, "ws-1", "task-1")
	if err != nil {
		t.Fatalf("taskWorktreeAssignment: %v", err)
	}
	if opts.Hub != nil {
		t.Errorf("Hub: got non-nil, want nil when DaemonHub is unset")
	}
	if opts.LocalPath != "" {
		t.Errorf("LocalPath: got %q, want empty (no local_directory routed)", opts.LocalPath)
	}
}

// TestRun_KeepAliveFiresDuringLongTask is the regression pin for
// the "大模型离线了" bug from issue 08f3b4a5-95df-45aa-a76f-a34a498666ff.
//
// Background: openai-http runtimes have no daemon-side heartbeat
// (their daemon_id is the synthetic `llm-…` string, and the daemon
// heartbeat scheduler in handler/daemon.go never touches them).
// The LLM worker's keep-alive pass — every KeepAliveInterval it
// pings /models and bumps last_seen_at — is the ONLY thing keeping
// the row alive. If the keep-alive pass is blocked by a long task
// for longer than the 150s stale window in
// cmd/server/runtime_sweeper.go, the sweeper flips the runtime to
// offline and FailTasksForOfflineRuntimes fails the in-flight
// task with reason=runtime_offline. The user sees "大模型离线了"
// in the runtime picker.
//
// The original implementation put the keep-alive ticker in the
// same for-loop as ExecuteOnce, so a 3-minute multi-turn tool
// task would starve the ticker and trigger exactly that failure
// mode (see agent_task_queue rows a74a8a8b / 1aa7714f in the
// issue's history).
//
// The fix moved the keep-alive into its own goroutine
// (runKeepAliveLoop). This test pins the new contract: while
// ExecuteOnce is blocked — simulating a long LLM call — the
// keep-alive goroutine must continue to fire on its own cadence.
//
// The test uses the listOpenaiHTTPRuntimesFn / keepAliveFn
// function pointers on Worker so neither path needs a real DB
// connection; Run sees the stubs as if they were the production
// implementations.
func TestRun_KeepAliveFiresDuringLongTask(t *testing.T) {
	// 20ms tick → 5 ticks in 100ms of busy time → comfortable
	// margin for the "at least 3" assertion below even on a
	// heavily loaded CI runner. A real KeepAliveInterval
	// (60s) would make the test painfully slow without
	// exercising anything new.
	// A real secretbox is required so w.Enabled() returns
	// true; Run short-circuits to ErrDisabled otherwise and
	// never spawns the keep-alive goroutine, which would
	// produce a vacuous test.
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	w := NewWorker(nil, box, nil, nil, nil, discardLogger())
	w.KeepAliveInterval = 20 * time.Millisecond
	// PollInterval is irrelevant for this test (we never
	// reach the task ticker — listOpenaiHTTPRuntimesFn blocks
	// for the whole test) but set it to something explicit
	// so a future refactor that changes the order doesn't
	// accidentally make PollInterval the bottleneck.
	w.PollInterval = 1 * time.Hour
	var keepAliveCount int32
	w.keepAliveFn = func(ctx context.Context) {
		atomic.AddInt32(&keepAliveCount, 1)
	}
	// Block ExecuteOnce by holding listOpenaiHTTPRuntimesFn
	// until the test releases it. ctx is propagated so
	// cancelling the test still unblocks the inner call
	// promptly (no goroutine leak on test cleanup).
	releaseExecuteOnce := make(chan struct{})
	w.listOpenaiHTTPRuntimesFn = func(ctx context.Context) ([]db.AgentRuntime, error) {
		select {
		case <-releaseExecuteOnce:
		case <-ctx.Done():
		}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(runDone)
	}()
	// Give the keep-alive goroutine ~5 tick windows while the
	// main loop is parked inside ExecuteOnce. With the bug
	// this fires zero times; with the fix it fires >= 3.
	time.Sleep(100 * time.Millisecond)
	// Release the main loop and let Run exit.
	close(releaseExecuteOnce)
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel; goroutine leak?")
	}
	if got := atomic.LoadInt32(&keepAliveCount); got < 3 {
		t.Fatalf("keep-alive fired %d times while ExecuteOnce was blocked; want >= 3. "+
			"Repro for the 08f3b4a5 runtime_offline regression: a busy main loop must not starve the keep-alive goroutine.",
			got)
	}
}

// TestRunKeepAliveLoop_RespectsContextCancel is the companion
// contract: runKeepAliveLoop must exit promptly when the worker
// context is cancelled, so server shutdown doesn't leak a
// goroutine that's still trying to ping /models.
func TestRunKeepAliveLoop_RespectsContextCancel(t *testing.T) {
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	w := NewWorker(nil, box, nil, nil, nil, discardLogger())
	w.KeepAliveInterval = 10 * time.Millisecond
	// keepAliveFn that, if the goroutine is leaked, will
	// continue incrementing — easy to detect in the
	// post-cancel quiescence check below.
	var ticks int32
	w.keepAliveFn = func(ctx context.Context) {
		atomic.AddInt32(&ticks, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.runKeepAliveLoop(ctx)
		close(done)
	}()
	// Let it tick a few times so we know the loop body is
	// executing.
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runKeepAliveLoop did not exit within 1s of cancel")
	}
	// Snapshot after cancel and confirm no further ticks
	// land (would indicate the loop missed the cancel and
	// is running with a closed-over ctx).
	postCancel := atomic.LoadInt32(&ticks)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&ticks); got != postCancel {
		t.Errorf("keep-alive ticked %d more times after cancel (started at %d, now %d); loop did not honour ctx",
			got-postCancel, postCancel, got)
	}
}
