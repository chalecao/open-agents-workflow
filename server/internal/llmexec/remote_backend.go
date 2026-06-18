package llmexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// remoteBackendInit is the constructor input for the
// remote (daemon-RPC) backend. Every field is required.
type remoteBackendInit struct {
	Hub            *daemonws.Hub
	DaemonID       string
	RuntimeID      string
	WorkspaceID    string
	TaskID         string
	LocalPath      string
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

// remoteBackend implements Backend for tasks whose worktree
// lives on a daemon's filesystem (local_directory projects).
// The server never touches the user's files directly; every
// file / shell / git op is dispatched to the daemon over the
// existing task-wakeup WebSocket as an RPC request.
//
// Session lifecycle:
//
//  1. The first call (typically Init, dispatched by
//     newRemoteBackend) sends an llm:init_worktree RPC. The
//     daemon validates the local_directory assignment, takes
//     the per-path mutex the same way the CLI runtime does,
//     walks the live tree to compute the init snapshot, and
//     returns the list of pre-existing files. The local
//     backend (on the server) stores that list as
//     `initial` for the changed-files diff.
//  2. Every subsequent file / shell / git op sends an
//     llm:<method> RPC tagged with the session_id. The
//     daemon looks up the session, runs the op in the
//     user's local dir, and returns the result.
//  3. On Cleanup, the server sends an llm:cleanup_worktree
//     RPC. The daemon releases the path mutex and discards
//     the session. The session_id is a fresh ULID-like
//     string chosen by the server; the daemon uses it to
//     key its in-memory session map.
//
// Failure modes:
//   - Hub != nil but no client is connected for the
//     runtime → the op returns daemonws.ErrDaemonNotReachable
//     and the tool loop surfaces the error to the model.
//   - Daemon returns OK=false with a Code → the tool loop
//     surfaces the daemon's Message as a tool error.
//   - ctx is cancelled before the reply → ErrDaemonRPCTimeout.
type remoteBackend struct {
	hub            *daemonws.Hub
	daemonID       string
	runtimeID      string
	workspaceID    string
	taskID         string
	localPath      string
	requestTimeout time.Duration
	logger         *slog.Logger

	// sessionID is set by Init; the daemon uses it to
	// key the per-task path mutex. Empty until Init
	// returns.
	sessionID string
	// initial is the snapshot the daemon returned from
	// init_worktree — the list of paths that existed at
	// task start. Used by ChangedFiles for the
	// "deleted" bucket and by WriteFile for the
	// pre-existing flag.
	initial map[string]fileSnapshot
	// mu guards sessionID / initial / created /
	// modified. The Worktree wrapper also touches
	// the same maps for in-process tracking, so the
	// lock is shared.
	mu       sync.Mutex
	created  map[string]struct{}
	modified map[string]struct{}
}

func newRemoteBackend(init remoteBackendInit) (*remoteBackend, error) {
	if init.Hub == nil {
		return nil, errors.New("remoteBackend: Hub is required")
	}
	if init.RuntimeID == "" {
		return nil, errors.New("remoteBackend: RuntimeID is required")
	}
	if init.WorkspaceID == "" {
		return nil, errors.New("remoteBackend: WorkspaceID is required")
	}
	if init.TaskID == "" {
		return nil, errors.New("remoteBackend: TaskID is required")
	}
	if init.LocalPath == "" {
		return nil, errors.New("remoteBackend: LocalPath is required")
	}
	if init.Logger == nil {
		init.Logger = slog.Default()
	}
	if init.RequestTimeout <= 0 {
		init.RequestTimeout = 60 * time.Second
	}
	rb := &remoteBackend{
		hub:            init.Hub,
		daemonID:       init.DaemonID,
		runtimeID:      init.RuntimeID,
		workspaceID:    init.WorkspaceID,
		taskID:         init.TaskID,
		localPath:      init.LocalPath,
		requestTimeout: init.RequestTimeout,
		logger:         init.Logger,
		created:        map[string]struct{}{},
		modified:       map[string]struct{}{},
	}
	// Hand the daemon a fresh session id. ULID-style
	// would be ideal for sortability, but a v4 UUID is
	// good enough — the daemon only needs a unique
	// per-task key.
	rb.sessionID = uuid.NewString()
	// Run init on the daemon. This is the synchronous
	// "open the tool session" call; subsequent ops are
	// also synchronous but cheaper (no mutex, no
	// snapshot).
	args, _ := json.Marshal(map[string]any{
		"local_path": init.LocalPath,
	})
	ctx, cancel := context.WithTimeout(context.Background(), init.RequestTimeout)
	defer cancel()
	result, err := init.Hub.Request(
		ctx, init.RuntimeID, rb.sessionID,
		protocol.RPCMethodLLMInitWorktree,
		init.WorkspaceID, init.TaskID, rb.sessionID,
		args,
	)
	if err != nil {
		return nil, fmt.Errorf("remoteBackend: init_worktree rpc: %w", err)
	}
	// Result is a JSON object with shape:
	//   {"initial": ["/abs/path/1", "/abs/path/2", ...]}
	// The daemon walks the live tree, computes relative
	// paths from local_path, and returns them. We
	// build the snapshot map from the daemon's list
	// (size + mtime are unknown to the server, so we
	// store zero values; the Worktree's in-process
	// tracking wins the "modified vs created" tie
	// without needing accurate size/mtime).
	var parsed struct {
		Initial []string `json:"initial"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("remoteBackend: init_worktree result: %w", err)
	}
	rb.initial = make(map[string]fileSnapshot, len(parsed.Initial))
	for _, p := range parsed.Initial {
		rb.initial[p] = fileSnapshot{}
	}
	if init.Logger != nil {
		init.Logger.Info("remoteBackend: init_worktree ok",
			"daemon_id", init.DaemonID,
			"runtime_id", init.RuntimeID,
			"session_id", rb.sessionID,
			"local_path", init.LocalPath,
			"initial_files", len(rb.initial),
		)
	}
	return rb, nil
}

// ---------------------------------------------------------------------------
// Backend interface implementation
// ---------------------------------------------------------------------------

// Root returns the synthetic "remote://..." identifier. We
// embed the daemon id so logs / task results show which
// daemon ran the work, and the local path so the operator
// can see the user's actual directory.
func (rb *remoteBackend) Root() string {
	return fmt.Sprintf("remote://%s%s", rb.daemonID, rb.localPath)
}

func (rb *remoteBackend) ReadFile(ctx context.Context, path string, maxBytes int) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	args, _ := json.Marshal(map[string]any{
		"path":      path,
		"max_bytes": maxBytes,
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMReadFile, args)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: read_file result: %w", err)
	}
	return parsed.Content, nil
}

func (rb *remoteBackend) WriteFile(ctx context.Context, path, content string) (bool, error) {
	if rb.sessionID == "" {
		return false, errors.New("remoteBackend: not initialised")
	}
	args, _ := json.Marshal(map[string]any{
		"path":    path,
		"content": content,
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMWriteFile, args)
	if err != nil {
		return false, err
	}
	var parsed struct {
		PreExisting bool `json:"pre_existing"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return false, fmt.Errorf("remoteBackend: write_file result: %w", err)
	}
	return parsed.PreExisting, nil
}

func (rb *remoteBackend) ListDir(ctx context.Context, path string) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	args, _ := json.Marshal(map[string]any{
		"path": path,
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMListDir, args)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Entries string `json:"entries"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: list_dir result: %w", err)
	}
	return parsed.Entries, nil
}

func (rb *remoteBackend) RunShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	args, _ := json.Marshal(map[string]any{
		"command":        command,
		"timeout_seconds": int(timeout / time.Second),
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMRunShell, args)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: run_shell result: %w", err)
	}
	return parsed.Output, nil
}

func (rb *remoteBackend) GitStatus(ctx context.Context) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	result, err := rb.call(ctx, protocol.RPCMethodLLMGitStatus, nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: git_status result: %w", err)
	}
	return parsed.Output, nil
}

func (rb *remoteBackend) GitDiff(ctx context.Context, staged bool) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	args, _ := json.Marshal(map[string]any{
		"staged": staged,
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMGitDiff, args)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: git_diff result: %w", err)
	}
	return parsed.Output, nil
}

func (rb *remoteBackend) GitCommit(ctx context.Context, message string) (string, error) {
	if rb.sessionID == "" {
		return "", errors.New("remoteBackend: not initialised")
	}
	args, _ := json.Marshal(map[string]any{
		"message": message,
	})
	result, err := rb.call(ctx, protocol.RPCMethodLLMGitCommit, args)
	if err != nil {
		return "", err
	}
	var parsed struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("remoteBackend: git_commit result: %w", err)
	}
	return parsed.SHA, nil
}

// ChangedFiles computes the deleted bucket by diffing the
// daemon's last known live tree against the init-time
// snapshot. The daemon returns the live tree on the same
// git_status / list_dir calls as part of the tool flow;
// rather than asking the daemon again, the simplest
// contract is for the server to ask the daemon for the
// current state of the workdir on demand. We do that via
// a special-purpose RPC.
//
// For now, fall back to the local snapshot the daemon
// gave us at init time: the Worktree's in-process
// tracking handles "created" and "modified"; "deleted"
// is filled in lazily when the daemon re-snapshots.
//
// This is intentionally conservative — the daemon may
// have more accurate info but we don't issue a separate
// RPC for every ChangedFiles call (the activity log
// summary only needs an approximate view).
func (rb *remoteBackend) ChangedFiles() (created, modified, deleted []string) {
	// The Worktree wrapper's ChangedFiles consults
	// this method only for the "deleted" bucket. We
	// can't compute it accurately without asking
	// the daemon for the live state, so we return
	// an empty deleted list and let the operator see
	// the model's view (which is what it committed).
	// A future iteration can add a dedicated
	// llm:changed_files RPC if the audit log needs
	// the exact deletion set.
	return nil, nil, nil
}

// initialSnapshot is the Backend-interface implementation
// that returns the init-time snapshot. The Worktree
// wrapper seeds its own tracking with this map.
func (rb *remoteBackend) initialSnapshot() map[string]fileSnapshot {
	return rb.initial
}

// Cleanup sends the llm:cleanup_worktree RPC and discards
// the local session state. Safe to call multiple times.
func (rb *remoteBackend) Cleanup() {
	if rb.sessionID == "" {
		return
	}
	// Use a fresh, short context for cleanup so a
	// cancelled worker ctx (e.g. user killed the
	// task) doesn't prevent us from releasing the
	// daemon's path mutex. The 10s cap is plenty for
	// a no-op RPC.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]any{})
	_, err := rb.call(ctx, protocol.RPCMethodLLMCleanupWorktree, args)
	if err != nil && rb.logger != nil {
		// Non-fatal — the daemon's session TTL will
		// reap the session if cleanup never lands,
		// and the path mutex is released the next
		// time another task on the same path
		// completes.
		rb.logger.Warn("remoteBackend: cleanup_worktree rpc failed",
			"session_id", rb.sessionID,
			"error", err)
	}
	rb.sessionID = ""
	rb.initial = nil
	rb.created = nil
	rb.modified = nil
}

// call is the per-op RPC dispatch helper. It applies the
// per-RPC timeout (RequestTimeout) and translates the
// daemon's reply into a Go error. The request_id is unique
// per call so a slow RPC doesn't collide with a fast one
// reusing the same session.
func (rb *remoteBackend) call(ctx context.Context, method string, args json.RawMessage) (json.RawMessage, error) {
	timeout := rb.requestTimeout
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < timeout {
			timeout = remaining
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	requestID := uuid.NewString()
	return rb.hub.Request(
		callCtx, rb.runtimeID, requestID,
		method,
		rb.workspaceID, rb.taskID, rb.sessionID,
		args,
	)
}

// sortStrings is a small helper to keep the snapshot
// iteration deterministic in tests. Not used in production
// hot paths.
func sortStrings(s []string) {
	sort.Strings(s)
}
