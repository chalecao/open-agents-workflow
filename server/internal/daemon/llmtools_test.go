package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// newTestDaemonForLLMRPC constructs a minimal *Daemon sufficient
// for the llmRPCDispatcher. We don't stand up a full HTTP client
// (the dispatcher doesn't talk to the server) or a runtime
// registry; the LLM tool session is purely local fs + git work.
// rootCtx is what the dispatcher uses to acquire the path lock
// — it must outlive any test that calls into the dispatcher.
func newTestDaemonForLLMRPC(t *testing.T, rootCtx context.Context) (*Daemon, context.CancelFunc) {
	t.Helper()
	d := &Daemon{
		logger:         slog.Default(),
		localPathLocks: NewLocalPathLocker(),
	}
	d.rootCtx = rootCtx
	// llmRPC is normally wired in New(); for a hand-built
	// test Daemon we wire it explicitly.
	d.llmRPC = newLLMRPCDispatcher(d)
	cancel := func() {
		// The test owns rootCtx; nothing to do here
		// beyond signalling that the helper is being
		// torn down. Kept as a CancelFunc so the
		// call site reads the same way as a typical
		// context-with-cancel test fixture.
	}
	return d, cancel
}

// TestLLMRPCDispatcher_RegisterAllMethods confirms every
// protocol.RPCMethod* constant the server might dispatch is
// registered with the daemon. A missing handler is the worst
// possible failure mode: the server's tool loop calls into a
// method the daemon doesn't know, the model sees a generic
// "unknown_method" error, and the user-visible "all the LLM
// worker tools just stopped working" complaint hits support
// before the constant ever gets caught in code review.
func TestLLMRPCDispatcher_RegisterAllMethods(t *testing.T) {
	d, _ := newTestDaemonForLLMRPC(t, context.Background())
	want := []string{
		protocol.RPCMethodLLMInitWorktree,
		protocol.RPCMethodLLMCleanupWorktree,
		protocol.RPCMethodLLMReadFile,
		protocol.RPCMethodLLMWriteFile,
		protocol.RPCMethodLLMListDir,
		protocol.RPCMethodLLMRunShell,
		protocol.RPCMethodLLMGitStatus,
		protocol.RPCMethodLLMGitDiff,
		protocol.RPCMethodLLMGitCommit,
	}
	for _, m := range want {
		if _, ok := d.llmRPC.handlers[m]; !ok {
			t.Errorf("daemon-side rpc handler not registered for %q", m)
		}
	}
}

// TestLLMRPCDispatcher_UnknownMethodRejected pins the wire
// contract: a method name that doesn't appear in the registry
// must be rejected with code="unknown_method" so the server
// (and ultimately the LLM tool loop) can distinguish a
// protocol mismatch from a tool-level error.
func TestLLMRPCDispatcher_UnknownMethodRejected(t *testing.T) {
	d, _ := newTestDaemonForLLMRPC(t, context.Background())
	_, te := d.llmRPC.dispatch(
		context.Background(),
		"llm:nonexistent_method",
		"ws-1", "task-1", "sess-1",
		json.RawMessage(`{}`),
	)
	if te == nil {
		t.Fatal("expected unknown_method error, got nil")
	}
	if te.Code != "unknown_method" {
		t.Errorf("code = %q, want unknown_method", te.Code)
	}
}

// TestLLMRPCDispatcher_RequiresSessionForOps pins the gating
// rule: every method other than init_worktree must arrive with
// a non-empty session_id. Without this check, an op could
// slip through to a handler that expected a bound session and
// panic on the nil deref.
func TestLLMRPCDispatcher_RequiresSessionForOps(t *testing.T) {
	d, _ := newTestDaemonForLLMRPC(t, context.Background())
	cases := []string{
		protocol.RPCMethodLLMReadFile,
		protocol.RPCMethodLLMWriteFile,
		protocol.RPCMethodLLMListDir,
		protocol.RPCMethodLLMRunShell,
		protocol.RPCMethodLLMGitStatus,
		protocol.RPCMethodLLMGitDiff,
		protocol.RPCMethodLLMGitCommit,
		protocol.RPCMethodLLMCleanupWorktree,
	}
	for _, m := range cases {
		_, te := d.llmRPC.dispatch(
			context.Background(),
			m,
			"ws-1", "task-1", "",
			json.RawMessage(`{}`),
		)
		if te == nil {
			t.Errorf("%s: expected session_not_found error, got nil", m)
			continue
		}
		if te.Code != "session_not_found" {
			t.Errorf("%s: code = %q, want session_not_found", m, te.Code)
		}
	}
}

// TestLLMRPCDispatcher_UnknownSessionForOps pins the second
// half of the gating rule: a non-empty session_id that doesn't
// match any live session must be rejected, not silently
// re-initialised. The init_worktree path is the only legitimate
// way to create a session.
func TestLLMRPCDispatcher_UnknownSessionForOps(t *testing.T) {
	d, _ := newTestDaemonForLLMRPC(t, context.Background())
	_, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMReadFile,
		"ws-1", "task-1", "bogus-session",
		json.RawMessage(`{"path":"a.txt"}`),
	)
	if te == nil {
		t.Fatal("expected session_not_found error, got nil")
	}
	if te.Code != "session_not_found" {
		t.Errorf("code = %q, want session_not_found", te.Code)
	}
}

// TestLLMRPCDispatcher_InitAndFileOpsEndToEnd exercises the
// full session lifecycle through the dispatcher. It walks:
//
//  1. init_worktree with a real temp dir, takes the per-path
//     mutex, computes the snapshot, returns the initial list.
//  2. write_file to a new path inside the workdir.
//  3. read_file the same path back; the daemon reads the file
//     off disk and returns its content.
//  4. list_dir at the root, confirms the new file is visible.
//  5. cleanup_worktree releases the path lock and discards
//     the session.
//
// The test uses real fs + real path locks (not mocks) because
// the dispatcher's whole purpose is to safely route RPCs to
// local operations; mocking the operation would be testing
// the mock, not the code.
func TestLLMRPCDispatcher_InitAndFileOpsEndToEnd(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	d, _ := newTestDaemonForLLMRPC(t, rootCtx)
	dir := t.TempDir()
	const sessionID = "test-session-1"

	// 1. init_worktree
	res, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMInitWorktree,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{"local_path": dir}),
	)
	if te != nil {
		t.Fatalf("init_worktree: code=%s msg=%s", te.Code, te.Message)
	}
	var initResult struct {
		Initial []string `json:"initial"`
	}
	if err := json.Unmarshal(res, &initResult); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	// The temp dir is empty at this point.
	if len(initResult.Initial) != 0 {
		t.Errorf("init: initial = %v, want empty", initResult.Initial)
	}

	// 2. write_file
	_, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMWriteFile,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{
			"path":    "hello.txt",
			"content": "hi from test\n",
		}),
	)
	if te != nil {
		t.Fatalf("write_file: code=%s msg=%s", te.Code, te.Message)
	}
	// Confirm the file landed on disk.
	if data, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("readFile back: %v", err)
	} else if string(data) != "hi from test\n" {
		t.Errorf("content: got %q, want %q", data, "hi from test\n")
	}

	// 3. read_file
	res, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMReadFile,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{"path": "hello.txt"}),
	)
	if te != nil {
		t.Fatalf("read_file: code=%s msg=%s", te.Code, te.Message)
	}
	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(res, &readResult); err != nil {
		t.Fatalf("unmarshal read result: %v", err)
	}
	if readResult.Content != "hi from test\n" {
		t.Errorf("read_file: got %q", readResult.Content)
	}

	// 4. list_dir
	res, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMListDir,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{"path": "."}),
	)
	if te != nil {
		t.Fatalf("list_dir: code=%s msg=%s", te.Code, te.Message)
	}
	var listResult struct {
		Entries string `json:"entries"`
	}
	if err := json.Unmarshal(res, &listResult); err != nil {
		t.Fatalf("unmarshal list result: %v", err)
	}
	if !strings.Contains(listResult.Entries, "hello.txt") {
		t.Errorf("list_dir entries missing hello.txt: %q", listResult.Entries)
	}

	// 5. cleanup_worktree
	_, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMCleanupWorktree,
		"ws-1", "task-1", sessionID,
		json.RawMessage(`{}`),
	)
	if te != nil {
		t.Fatalf("cleanup_worktree: code=%s msg=%s", te.Code, te.Message)
	}
	// After cleanup, the session is gone — a follow-up
	// op must fail with session_not_found.
	_, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMReadFile,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{"path": "hello.txt"}),
	)
	if te == nil || te.Code != "session_not_found" {
		t.Errorf("post-cleanup read_file: te=%+v, want session_not_found", te)
	}
	// And the path mutex is released — a fresh init
	// on the same path must succeed without blocking.
	res, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMInitWorktree,
		"ws-1", "task-1", "test-session-2",
		mustJSON(t, map[string]any{"local_path": dir}),
	)
	if te != nil {
		t.Fatalf("re-init after cleanup: code=%s msg=%s", te.Code, te.Message)
	}
	if res == nil {
		t.Error("re-init returned nil result")
	}
}

// TestLLMRPCDispatcher_InitBadArgs pins the input-validation
// path: a missing or empty local_path must be rejected with
// code="bad_args" before the daemon touches the filesystem or
// acquires any lock. Same for the missing session_id.
func TestLLMRPCDispatcher_InitBadArgs(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	d, _ := newTestDaemonForLLMRPC(t, rootCtx)

	cases := []struct {
		name      string
		sessionID string
		args      string
	}{
		{"empty local_path", "s1", `{"local_path":""}`},
		{"missing local_path key", "s1", `{}`},
		{"empty session_id", "", `{"local_path":"/tmp"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, te := d.llmRPC.dispatch(
				context.Background(),
				protocol.RPCMethodLLMInitWorktree,
				"ws-1", "task-1", tc.sessionID,
				json.RawMessage(tc.args),
			)
			if te == nil {
				t.Fatal("expected bad_args error, got nil")
			}
			if te.Code != "bad_args" {
				t.Errorf("code = %q, want bad_args", te.Code)
			}
		})
	}
}

// TestLLMRPCDispatcher_ConcurrentInitOnSamePathBlocks pins
// the per-path mutex contract: two init_worktree calls on the
// same real path must serialise. The first holds the lock
// until its cleanup; the second blocks in Acquire until the
// first releases. We assert the second op completes after the
// first, even when started in a goroutine.
func TestLLMRPCDispatcher_ConcurrentInitOnSamePathBlocks(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	d, _ := newTestDaemonForLLMRPC(t, rootCtx)
	dir := t.TempDir()

	// First session holds the path.
	_, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMInitWorktree,
		"ws-1", "task-1", "session-A",
		mustJSON(t, map[string]any{"local_path": dir}),
	)
	if te != nil {
		t.Fatalf("first init: %+v", te)
	}
	defer func() {
		_, _ = d.llmRPC.dispatch(
			context.Background(),
			protocol.RPCMethodLLMCleanupWorktree,
			"ws-1", "task-1", "session-A",
			json.RawMessage(`{}`),
		)
	}()

	// Second init on the same path. The path lock
	// serialises — the second call must block in
	// Acquire. We run it in a goroutine and assert it
	// doesn't return until we release the first lock.
	var finishedAt atomic.Int64
	done := make(chan struct{})
	go func() {
		_, _ = d.llmRPC.dispatch(
			context.Background(),
			protocol.RPCMethodLLMInitWorktree,
			"ws-1", "task-2", "session-B",
			mustJSON(t, map[string]any{"local_path": dir}),
		)
		finishedAt.Store(time.Now().UnixNano())
		close(done)
	}()
	// Give the goroutine time to enter the Acquire wait.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("second init returned before the first released the path lock")
	default:
	}
	// Now release the first lock by sending cleanup; the
	// second op should unblock shortly after.
	_, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMCleanupWorktree,
		"ws-1", "task-1", "session-A",
		json.RawMessage(`{}`),
	)
	if te != nil {
		t.Fatalf("cleanup: %+v", te)
	}
	select {
	case <-done:
		// The mutex is the only thing that could have
		// blocked the second init. Confirming the
		// timestamp is positive is enough — if the
		// path lock didn't serialise, done would
		// have closed within the 200ms sleep above.
		if finishedAt.Load() == 0 {
			t.Fatal("second init did not record a finish timestamp")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second init never returned after first cleanup")
	}
}

// TestLLMRPCDispatcher_RunShellExercisesWorkdir confirms the
// shell handler runs the command with cwd=worktree. A simple
// `pwd` returns the workdir's absolute path; the test compares
// it to filepath.Clean(dir) to be robust against the daemon
// normalising the path (resolveRealPath, etc.).
func TestLLMRPCDispatcher_RunShellExercisesWorkdir(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	d, _ := newTestDaemonForLLMRPC(t, rootCtx)
	dir := t.TempDir()
	_, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMInitWorktree,
		"ws-1", "task-1", "shell-sess",
		mustJSON(t, map[string]any{"local_path": dir}),
	)
	if te != nil {
		t.Fatalf("init: %+v", te)
	}
	defer func() {
		_, _ = d.llmRPC.dispatch(
			context.Background(),
			protocol.RPCMethodLLMCleanupWorktree,
			"ws-1", "task-1", "shell-sess",
			json.RawMessage(`{}`),
		)
	}()
	res, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMRunShell,
		"ws-1", "task-1", "shell-sess",
		mustJSON(t, map[string]any{
			"command":        "pwd",
			"timeout_seconds": 5,
		}),
	)
	if te != nil {
		t.Fatalf("run_shell: %+v", te)
	}
	var runResult struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(res, &runResult); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Resolve symlinks (macOS /var → /private/var is the
	// usual culprit) and compare.
	got, err := filepath.EvalSymlinks(strings.TrimSpace(runResult.Output))
	if err != nil {
		t.Fatalf("EvalSymlinks(got): %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

// mustJSON is a small test helper for marshaling test args.
// Centralised so the call sites read as the test intent
// (a map of args) without the boilerplate.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// recoveredSessionsCount returns the number of active llm
// sessions in the dispatcher. We don't expose this in
// production; it lives here as a test-only assertion helper
// for the cleanup_worktree test.
func recoveredSessionsCount(d *Daemon) int {
	count := 0
	d.llmRPC.sessions.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// TestLLMRPCDispatcher_CleanupRemovesSession confirms the
// session map drops the entry on cleanup_worktree, so a
// follow-up op with the same session_id correctly fails with
// session_not_found. The map size is a tighter contract than
// "the op fails" because it also catches a leak (e.g. a
// future bug where the dispatcher never deletes the entry).
func TestLLMRPCDispatcher_CleanupRemovesSession(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	d, _ := newTestDaemonForLLMRPC(t, rootCtx)
	dir := t.TempDir()
	const sessionID = "leak-check"

	_, te := d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMInitWorktree,
		"ws-1", "task-1", sessionID,
		mustJSON(t, map[string]any{"local_path": dir}),
	)
	if te != nil {
		t.Fatalf("init: %+v", te)
	}
	if n := recoveredSessionsCount(d); n != 1 {
		t.Fatalf("after init: sessions = %d, want 1", n)
	}
	_, te = d.llmRPC.dispatch(
		context.Background(),
		protocol.RPCMethodLLMCleanupWorktree,
		"ws-1", "task-1", sessionID,
		json.RawMessage(`{}`),
	)
	if te != nil {
		t.Fatalf("cleanup: %+v", te)
	}
	if n := recoveredSessionsCount(d); n != 0 {
		t.Fatalf("after cleanup: sessions = %d, want 0 (leak)", n)
	}
}
