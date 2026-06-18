package llmexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeDaemon is a per-test fake daemon WebSocket peer. It registers
// itself with the supplied hub for a single runtime, reads every
// inbound frame, dispatches to a handler map, and writes back a
// rpc_response. Tests can assert on the recorded frames to confirm
// the server sent the right request shape.
type fakeDaemon struct {
	t          *testing.T
	hub        *daemonws.Hub
	runtimeID  string
	conn       *websocket.Conn
	handler    func(req protocol.DaemonRPCRequestPayload) (protocol.DaemonRPCResponsePayload, bool)
	mu         sync.Mutex
	received   []protocol.DaemonRPCRequestPayload
	requestsCh chan protocol.DaemonRPCRequestPayload
}

func newFakeDaemon(t *testing.T, hub *daemonws.Hub, runtimeID string, handler func(req protocol.DaemonRPCRequestPayload) (protocol.DaemonRPCResponsePayload, bool)) *fakeDaemon {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWebSocket(w, r, daemonws.ClientIdentity{RuntimeIDs: []string{runtimeID}})
	}))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Wait for the hub to register the connection so the first
	// Request() from the server doesn't race the register.
	deadline := time.Now().Add(time.Second)
	for hub.RuntimeConnectionCount(runtimeID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon connection was not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	fd := &fakeDaemon{
		t:          t,
		hub:        hub,
		runtimeID:  runtimeID,
		conn:       conn,
		handler:    handler,
		requestsCh: make(chan protocol.DaemonRPCRequestPayload, 16),
	}
	go fd.readLoop()
	return fd
}

// readLoop pulls frames off the WebSocket, dispatches to the
// handler, and writes the response back. A nil handler is
// treated as "always OK with the empty JSON object" so a test
// that only cares about request shape doesn't have to write
// the response boilerplate. The loop exits when the connection
// read fails (e.g. the test's t.Cleanup closed it).
func (fd *fakeDaemon) readLoop() {
	for {
		_ = fd.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, raw, err := fd.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg protocol.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Type != protocol.EventDaemonRPCRequest {
			continue
		}
		var req protocol.DaemonRPCRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			continue
		}
		fd.mu.Lock()
		fd.received = append(fd.received, req)
		fd.mu.Unlock()
		// Surface the request to the test synchronously so
		// a test that calls `expect(<-requestsCh)` can
		// keep the test deterministic without polling.
		select {
		case fd.requestsCh <- req:
		default:
		}
		var resp protocol.DaemonRPCResponsePayload
		if fd.handler != nil {
			resp, _ = fd.handler(req)
		} else {
			resp = protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{}`)}
		}
		resp.RequestID = req.RequestID
		frame, _ := json.Marshal(protocol.Message{
			Type:    protocol.EventDaemonRPCResponse,
			Payload: mustMarshalRaw(resp),
		})
		_ = fd.conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := fd.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			return
		}
	}
}

// snapshot returns a copy of the requests received so far.
func (fd *fakeDaemon) snapshot() []protocol.DaemonRPCRequestPayload {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	out := make([]protocol.DaemonRPCRequestPayload, len(fd.received))
	copy(out, fd.received)
	return out
}

// TestRemoteBackend_InitCallsInitWorktreeRPC is the happy-path
// test for the new RemoteBackend. It confirms the constructor:
//   - sends an llm:init_worktree RPC on construction
//   - waits for the daemon's reply
//   - stores the daemon's "initial" list as the snapshot
//   - records a non-empty session_id
//   - exposes the synthetic "remote://..." root to the Worktree
func TestRemoteBackend_InitCallsInitWorktreeRPC(t *testing.T) {
	hub := daemonws.NewHub()
	daemon := newFakeDaemon(t, hub, "rt-1", func(req protocol.DaemonRPCRequestPayload) (protocol.DaemonRPCResponsePayload, bool) {
		return protocol.DaemonRPCResponsePayload{
			OK:     true,
			Result: json.RawMessage(`{"initial":["a.txt","sub/b.txt"]}`),
		}, true
	})

	rb, err := newRemoteBackend(remoteBackendInit{
		Hub:            hub,
		DaemonID:       "daemon-1",
		RuntimeID:      "rt-1",
		WorkspaceID:    "ws-1",
		TaskID:         "task-1",
		LocalPath:      "/Users/foo/proj",
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newRemoteBackend: %v", err)
	}
	defer rb.Cleanup()

	if rb.sessionID == "" {
		t.Fatal("expected non-empty session_id after init")
	}
	if got := rb.Root(); !strings.HasPrefix(got, "remote://") {
		t.Errorf("Root() = %q, want remote:// prefix", got)
	}
	snap := rb.initialSnapshot()
	if _, ok := snap["a.txt"]; !ok {
		t.Errorf("initial snapshot missing a.txt: %+v", snap)
	}
	if _, ok := snap["sub/b.txt"]; !ok {
		t.Errorf("initial snapshot missing sub/b.txt: %+v", snap)
	}
	// Wait for the init RPC to be received, then assert on
	// the wire shape. The fake daemon's requestsCh
	// serialises every request, so the first entry is
	// necessarily the init call (the test does no other RPC
	// between the constructor and this read).
	select {
	case req := <-daemon.requestsCh:
		if req.Method != protocol.RPCMethodLLMInitWorktree {
			t.Errorf("first method = %q, want %q", req.Method, protocol.RPCMethodLLMInitWorktree)
		}
		if req.WorkspaceID != "ws-1" || req.TaskID != "task-1" {
			t.Errorf("request scope = (%q,%q), want (ws-1,task-1)", req.WorkspaceID, req.TaskID)
		}
		var args struct {
			LocalPath string `json:"local_path"`
		}
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatalf("unmarshal args: %v", err)
		}
		if args.LocalPath != "/Users/foo/proj" {
			t.Errorf("local_path arg = %q, want /Users/foo/proj", args.LocalPath)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon never received init_worktree RPC")
	}
}

// TestRemoteBackend_DispatchesFileShellGitOps exercises every
// non-init op through the Worktree, confirms each one fires the
// expected RPC, and confirms the result is parsed into the
// expected Go value (pre_existing, output, sha, …). The script
// in the fake daemon is the only source of truth — if a tool
// forgets to call the backend, the test fails on the missing
// request.
func TestRemoteBackend_DispatchesFileShellGitOps(t *testing.T) {
	hub := daemonws.NewHub()
	const wantSHA = "abcdef1"
	var unknownMethod atomic.Value
	daemon := newFakeDaemon(t, hub, "rt-1", func(req protocol.DaemonRPCRequestPayload) (protocol.DaemonRPCResponsePayload, bool) {
		switch req.Method {
		case protocol.RPCMethodLLMInitWorktree:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"initial":[]}`)}, true
		case protocol.RPCMethodLLMWriteFile:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"pre_existing":true}`)}, true
		case protocol.RPCMethodLLMReadFile:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"content":"hello\n"}`)}, true
		case protocol.RPCMethodLLMListDir:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"entries":"a.txt\nb/\n"}`)}, true
		case protocol.RPCMethodLLMRunShell:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"output":"shell output"}`)}, true
		case protocol.RPCMethodLLMGitStatus:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"output":" M a.txt"}`)}, true
		case protocol.RPCMethodLLMGitDiff:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"output":"diff --git a M"}`)}, true
		case protocol.RPCMethodLLMGitCommit:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"sha":"` + wantSHA + `"}`)}, true
		case protocol.RPCMethodLLMCleanupWorktree:
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{}`)}, true
		default:
			// Stash the unknown method so the test can
			// assert on it (the handler runs in a
			// goroutine and t.Errorf from a goroutine
			// races the test's main assertions).
			unknownMethod.Store(req.Method)
			return protocol.DaemonRPCResponsePayload{OK: false, Error: "unknown"}, true
		}
	})

	rb, err := newRemoteBackend(remoteBackendInit{
		Hub:            hub,
		DaemonID:       "daemon-1",
		RuntimeID:      "rt-1",
		WorkspaceID:    "ws-1",
		TaskID:         "task-1",
		LocalPath:      "/Users/foo/proj",
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newRemoteBackend: %v", err)
	}

	ctx := context.Background()
	// write_file: pre_existing=true is what the daemon said, so
	// the Worktree should record the path in modified, not
	// created.
	preExisting, err := rb.WriteFile(ctx, "a.txt", "hello")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !preExisting {
		t.Error("WriteFile: pre_existing should be true")
	}
	// read_file
	body, err := rb.ReadFile(ctx, "a.txt", 100)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if body != "hello\n" {
		t.Errorf("ReadFile: got %q, want %q", body, "hello\n")
	}
	// list_dir
	entries, err := rb.ListDir(ctx, ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if entries != "a.txt\nb/\n" {
		t.Errorf("ListDir: got %q", entries)
	}
	// run_shell
	out, err := rb.RunShell(ctx, "ls", time.Second)
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if out != "shell output" {
		t.Errorf("RunShell: got %q", out)
	}
	// git ops
	if status, err := rb.GitStatus(ctx); err != nil || status != " M a.txt" {
		t.Errorf("GitStatus: got %q err=%v", status, err)
	}
	if diff, err := rb.GitDiff(ctx, false); err != nil || diff != "diff --git a M" {
		t.Errorf("GitDiff: got %q err=%v", diff, err)
	}
	sha, err := rb.GitCommit(ctx, "msg")
	if err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	if sha != wantSHA {
		t.Errorf("GitCommit sha: got %q, want %q", sha, wantSHA)
	}
	rb.Cleanup()

	// Drain the recorded requests and confirm every method
	// fired exactly once (init + 6 ops + cleanup = 8).
	got := daemon.snapshot()
	want := map[string]int{
		protocol.RPCMethodLLMInitWorktree:    1,
		protocol.RPCMethodLLMWriteFile:       1,
		protocol.RPCMethodLLMReadFile:        1,
		protocol.RPCMethodLLMListDir:         1,
		protocol.RPCMethodLLMRunShell:        1,
		protocol.RPCMethodLLMGitStatus:       1,
		protocol.RPCMethodLLMGitDiff:         1,
		protocol.RPCMethodLLMGitCommit:       1,
		protocol.RPCMethodLLMCleanupWorktree: 1,
	}
	gotCount := map[string]int{}
	for _, r := range got {
		gotCount[r.Method]++
	}
	for m, n := range want {
		if gotCount[m] != n {
			t.Errorf("method %q: got %d calls, want %d", m, gotCount[m], n)
		}
	}
	if v := unknownMethod.Load(); v != nil {
		t.Errorf("handler saw unexpected method %q", v)
	}
}

// TestRemoteBackend_PropagatesDaemonError confirms that an
// OK=false reply with a Code bubbles up as a *daemonws.ErrDaemonRPCFailed
// to the caller. The LLM tool loop surfaces that to the model as
// a tool error so it can self-correct.
func TestRemoteBackend_PropagatesDaemonError(t *testing.T) {
	hub := daemonws.NewHub()
	newFakeDaemon(t, hub, "rt-1", func(req protocol.DaemonRPCRequestPayload) (protocol.DaemonRPCResponsePayload, bool) {
		if req.Method == protocol.RPCMethodLLMInitWorktree {
			return protocol.DaemonRPCResponsePayload{OK: true, Result: json.RawMessage(`{"initial":[]}`)}, true
		}
		return protocol.DaemonRPCResponsePayload{
			OK:    false,
			Code:  "shell_timeout",
			Error: "command did not finish in 30s",
		}, true
	})

	rb, err := newRemoteBackend(remoteBackendInit{
		Hub:            hub,
		RuntimeID:      "rt-1",
		WorkspaceID:    "ws-1",
		TaskID:         "task-1",
		LocalPath:      "/Users/foo/proj",
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newRemoteBackend: %v", err)
	}
	defer rb.Cleanup()

	_, err = rb.RunShell(context.Background(), "true", time.Second)
	if err == nil {
		t.Fatal("expected error from daemon OK=false reply")
	}
	if !strings.Contains(err.Error(), "shell_timeout") {
		t.Errorf("error = %q, want contains shell_timeout code", err.Error())
	}
}

// TestRemoteBackend_NotReachableWhenNoClient confirms the
// transport error path: if the daemon disconnects before the
// next op, RunShell returns ErrDaemonNotReachable, not a hang.
// The fake daemon in this test is closed at registration time
// (so the next call sees zero clients).
func TestRemoteBackend_NotReachableWhenNoClient(t *testing.T) {
	hub := daemonws.NewHub()
	// Brief fake daemon that just registers + immediately
	// disconnects. The first Request (init) will fail.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWebSocket(w, r, daemonws.ClientIdentity{RuntimeIDs: []string{"rt-1"}})
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Wait for the hub to register the conn, then close it.
	deadline := time.Now().Add(time.Second)
	for hub.RuntimeConnectionCount("rt-1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon connection was not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn.Close()
	// Give the unregister goroutine time to run.
	time.Sleep(50 * time.Millisecond)

	_, err = newRemoteBackend(remoteBackendInit{
		Hub:            hub,
		RuntimeID:      "rt-1",
		WorkspaceID:    "ws-1",
		TaskID:         "task-1",
		LocalPath:      "/Users/foo/proj",
		RequestTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected init to fail when no client is reachable")
	}
	if !strings.Contains(err.Error(), "rpc") {
		t.Errorf("error = %q, want contains 'rpc'", err.Error())
	}
}

// mustMarshalRaw is a small helper used by the fake daemon. It's
// the same helper the real hub uses (mustMarshalRaw in hub.go),
// but redefined here so this test file doesn't need to import
// internal symbols just to build an outbound frame.
func mustMarshalRaw(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
