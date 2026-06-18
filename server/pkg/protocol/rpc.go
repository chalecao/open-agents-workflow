package protocol

import "encoding/json"

// RPCMethod identifiers for daemon RPC requests.
//
// The daemon-side dispatch table maps these strings to handler functions in
// internal/daemon/llmtools. Keep this list in sync with the registered
// handlers on both sides; an unknown method from the server is rejected with
// code="unknown_method" rather than silently dropped.
const (
	// RPCMethodLLMInitWorktree asks the daemon to open a tool session
	// pinned to a (workspaceID, taskID) tuple inside the local_directory
	// path. The daemon acquires the per-path mutex the same way the CLI
	// runtime does, takes a snapshot of the working tree for diff
	// tracking, and returns a session_id the server uses for every
	// subsequent op.
	RPCMethodLLMInitWorktree = "llm:init_worktree"
	// RPCMethodLLMCleanupWorktree asks the daemon to release the path
	// mutex and discard the session. The diff between the initial
	// snapshot and the live working tree is returned so the server can
	// surface it in tool_audit.
	RPCMethodLLMCleanupWorktree = "llm:cleanup_worktree"
	// RPCMethodLLMReadFile / WriteFile / ListDir are the basic
	// filesystem ops. All paths are relative to the worktree root and
	// are sanitised server-side before the RPC fires.
	RPCMethodLLMReadFile  = "llm:read_file"
	RPCMethodLLMWriteFile = "llm:write_file"
	RPCMethodLLMListDir   = "llm:list_dir"
	// RPCMethodLLMRunShell runs an arbitrary command in the worktree
	// with the same per-call timeout the local Worktree uses. The
	// daemon validates the cwd is the worktree root so a hostile
	// payload can't escape the sandbox.
	RPCMethodLLMRunShell = "llm:run_shell"
	// RPCMethodLLMGitStatus / GitDiff / GitCommit wrap `git status`,
	// `git diff`, and `git commit` against the worktree. They share the
	// snapshot the daemon took in init_worktree so the diff the model
	// sees is consistent across calls.
	RPCMethodLLMGitStatus = "llm:git_status"
	RPCMethodLLMGitDiff   = "llm:git_diff"
	RPCMethodLLMGitCommit = "llm:git_commit"
)

// DaemonRPCRequestPayload is the body of a server-→-daemon RPC frame sent
// over the existing task-wakeup WebSocket. The daemon reads it, dispatches
// to a handler keyed by Method, and replies with DaemonRPCResponsePayload
// tagged with the same RequestID.
//
// The server multiplexes many in-flight requests over one connection by
// RequestID. The daemon can run handlers concurrently; each handler must be
// safe under that concurrency.
type DaemonRPCRequestPayload struct {
	// RequestID is a unique id (UUIDv4 or ULID) chosen by the server.
	// The daemon echoes it verbatim in the response. Required.
	RequestID string `json:"request_id"`
	// Method is one of the RPCMethod* constants above. Required.
	Method string `json:"method"`
	// WorkspaceID scopes the request to a workspace; the daemon uses
	// it to locate the project resource and validate that the local
	// directory the worktree pins to is still claimed by this daemon.
	WorkspaceID string `json:"workspace_id"`
	// TaskID scopes the request to a single task; the daemon uses it
	// for the path-mutex key so two concurrent LLM tasks on the same
	// workspace don't trample each other.
	TaskID string `json:"task_id"`
	// SessionID is set on every op after the matching init_worktree.
	// The daemon keys the path-mutex and the snapshot map on
	// SessionID. Empty on init_worktree itself.
	SessionID string `json:"session_id,omitempty"`
	// Args is the JSON-encoded argument struct for Method. Each
	// method defines its own shape (see internal/daemon/llmtools
	// handlers). Always an object, never a bare array/scalar.
	Args json.RawMessage `json:"args"`
}

// DaemonRPCResponsePayload is the body of a daemon-→-server RPC reply. The
// server matches it to the pending request by RequestID and unblocks the
// caller. Exactly one of Result or Error is set.
type DaemonRPCResponsePayload struct {
	// RequestID echoes the value the server sent. Required.
	RequestID string `json:"request_id"`
	// OK=true means Result is populated with the method's success
	// payload. OK=false means Error is populated and the call should
	// be considered a tool-level error (the model is informed via
	// role:"tool" with an "ERROR: " prefix so it can self-correct).
	OK bool `json:"ok"`
	// Result is the JSON-encoded success payload. Opaque shape per
	// method. Only set when OK=true.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is the human-readable failure reason. Only set when
	// OK=false. The model sees this string verbatim in the
	// role:"tool" message, so it should be short, actionable, and
	// free of internal stack frames.
	Error string `json:"error,omitempty"`
	// Code classifies the error so the server can route
	// dispatch-level errors (e.g. unknown_method, session_not_found)
	// distinctly from tool-level errors (e.g. file-not-found,
	// shell-timeout). Empty when OK=true.
	Code string `json:"code,omitempty"`
}
