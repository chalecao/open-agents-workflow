package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// RPC dispatch + handler registry for the LLM worker
// ---------------------------------------------------------------------------
//
// The server's llmexec worker (a long-lived OpenAI tool loop on the server)
// needs to drive filesystem / shell / git ops against a daemon's
// local_directory project. The transport is the same WebSocket the daemon
// already uses for heartbeats; the wire envelope is one frame per call
// (protocol.EventDaemonRPCRequest) with a server-chosen RequestID echoed in
// the EventDaemonRPCResponse reply.
//
// This file holds the daemon-side half of that protocol:
//
//   - llmRPCDispatcher: a per-Daemon singleton that owns the per-session
//     state and the handler registry. Initialised in New(); methods on the
//     Daemon reach for d.llmRPC to dispatch a request.
//
//   - llmSession: per-task state stored under the server-chosen
//     session_id. Holds the path-mutex releaser (so cleanup is a
//     one-liner), the resolved workdir root, and the init-time file
//     snapshot.
//
//   - handler funcs (initWorktree, readFile, writeFile, listDir,
//     runShell, gitStatus, gitDiff, gitCommit, cleanupWorktree): one
//     per protocol.RPCMethod* constant. They live as methods on
//     *llmRPCDispatcher so they can share the sessions map + path
//     mutex plumbing without globals.
//
//   - handleRPCRequest (in rpc_dispatch.go): the wire-level entry point
//     called by readTaskWakeupMessages. Parses the request, looks up
//     the handler, runs it under a context with the request's per-call
//     timeout, and serialises a response frame back over the same
//     WebSocket. Errors carry a Code so the server can distinguish
//     dispatch-level failures (unknown_method, session_not_found) from
//     tool-level errors (file-not-found, shell-timeout).

// llmSession is the per-task state the daemon keeps for the lifetime of
// an LLM tool session. Created by handleInitWorktree when the server
// sends the llm:init_worktree RPC; torn down by handleCleanupWorktree
// (or implicitly when the daemon shuts down — the path-mutex
// releaser is the only cleanup that matters in practice because the
// session map is bounded by the number of concurrent LLM tasks per
// workspace, which is small).
type llmSession struct {
	sessionID   string
	workspaceID string
	taskID      string
	runtimeID   string

	// realPath is the canonical (symlink-resolved) absolute path
	// the session is pinned to. Used as the key for
	// localPathLocks so two tasks on the same path serialise,
	// regardless of which symlink alias the user picked.
	realPath string
	// rootDir is the absolute path the model sees as the workdir
	// root. For local_directory projects that's the cleaned
	// user-supplied path (we never chdir into a worktree — the
	// user directory IS the workdir). Commands run with
	// cwd=rootDir.
	rootDir string
	// initial is the snapshot of every regular file (relative
	// path → {size, mtime}) taken at init time. Used to compute
	// the "deleted" bucket of the changed-files audit on
	// cleanup; the server-side Worktree uses this to decide
	// whether a file the model rewrote was a "create" or a
	// "modify".
	initial map[string]fileSnapshot
	// release releases the per-path mutex. Invoked from
	// cleanupWorktree. Idempotent.
	release func()
	// mu guards initial writes during snapshot extension
	// (currently unused — the snapshot is taken once and never
	// updated — but kept so future per-op snapshot patches don't
	// have to add a lock later).
	mu        sync.Mutex
	createdAt time.Time
}

// fileSnapshot is the daemon-side twin of the server's
// llmexec.fileSnapshot. The two structs are deliberately identical
// so the JSON wire shape for the init-worktree result is the same
// regardless of which side built the snapshot.
type fileSnapshot struct {
	size  int64
	mtime time.Time
}

// llmHandler is the per-method signature. The dispatcher wraps a
// caller with the session lookup and context plumbing; the handler
// itself just does the work and returns a JSON success payload or a
// *toolError. sessionID and taskID are passed separately from the
// session itself because the init handler needs to read them BEFORE
// the session exists; taskID is the per-path mutex key the lock
// requires (LocalPathLocker.Acquire refuses an empty taskID).
type llmHandler func(ctx context.Context, d *Daemon, sess *llmSession, taskID, sessionID string, args json.RawMessage) (json.RawMessage, error)

// llmRPCDispatcher is the per-Daemon RPC handler table for the LLM
// tool session. Methods map 1:1 to the protocol.RPCMethod* constants;
// new methods are added by extending the handlers map in
// newLLMRPCDispatcher.
type llmRPCDispatcher struct {
	d        *Daemon
	handlers map[string]llmHandler
	// sessions is keyed by session_id (server-chosen UUID). The
	// value is the per-task state. Access is goroutine-safe; the
	// cleanup_worktree handler deletes the entry on session end.
	sessions sync.Map
}

func newLLMRPCDispatcher(d *Daemon) *llmRPCDispatcher {
	r := &llmRPCDispatcher{d: d, handlers: make(map[string]llmHandler)}
	// One handler per RPC method. Grouped by lifecycle: init +
	// cleanup bracket the session; the four file/shell/git ops in
	// between are independent and may be served concurrently.
	r.handlers[protocol.RPCMethodLLMInitWorktree] = r.handleInitWorktree
	r.handlers[protocol.RPCMethodLLMCleanupWorktree] = r.handleCleanupWorktree
	r.handlers[protocol.RPCMethodLLMReadFile] = r.handleReadFile
	r.handlers[protocol.RPCMethodLLMWriteFile] = r.handleWriteFile
	r.handlers[protocol.RPCMethodLLMListDir] = r.handleListDir
	r.handlers[protocol.RPCMethodLLMRunShell] = r.handleRunShell
	r.handlers[protocol.RPCMethodLLMGitStatus] = r.handleGitStatus
	r.handlers[protocol.RPCMethodLLMGitDiff] = r.handleGitDiff
	r.handlers[protocol.RPCMethodLLMGitCommit] = r.handleGitCommit
	return r
}

// dispatch runs the handler for method with the parsed args. Returns
// the success payload (already JSON-encoded) and a toolError that
// distinguishes a method-level failure (no session, unknown method)
// from a tool-level failure (file-not-found, shell-timeout). The
// caller (handleRPCRequest) translates toolError into the wire
// response.
func (r *llmRPCDispatcher) dispatch(
	ctx context.Context,
	method string,
	workspaceID string,
	taskID string,
	sessionID string,
	args json.RawMessage,
) (json.RawMessage, *toolError) {
	handler, ok := r.handlers[method]
	if !ok {
		return nil, &toolError{Code: "unknown_method", Message: "unknown rpc method: " + method}
	}
	var sess *llmSession
	if sessionID != "" {
		v, ok := r.sessions.Load(sessionID)
		if !ok {
			// init_worktree is allowed through — the
			// server is creating a fresh session, so
			// "no entry yet" is the expected state, not
			// an error. The handler creates the entry.
			if method != protocol.RPCMethodLLMInitWorktree {
				return nil, &toolError{Code: "session_not_found", Message: "no active session for id " + sessionID}
			}
		} else {
			sess = v.(*llmSession)
		}
	} else if method != protocol.RPCMethodLLMInitWorktree {
		// Every method other than init must carry a
		// session_id. Reject anything else so a stray call
		// doesn't slip through to a handler that expects a
		// bound session.
		return nil, &toolError{Code: "session_not_found", Message: "session_id is required for " + method}
	}
	result, err := handler(ctx, r.d, sess, taskID, sessionID, args)
	if err != nil {
		// A handler may return either a *toolError (with a
		// Code) or a plain error. Plain errors get
		// Code="internal".
		var te *toolError
		if errors.As(err, &te) {
			return nil, te
		}
		return nil, &toolError{Code: "internal", Message: err.Error()}
	}
	return result, nil
}

// toolError is the per-method failure type. Code classifies the
// failure so the server can route dispatch errors distinctly from
// tool errors; Message is the human-readable reason that becomes the
// "ERROR: " prefix the LLM sees.
type toolError struct {
	Code    string
	Message string
}

func (e *toolError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func toolErr(code, msg string) *toolError {
	return &toolError{Code: code, Message: msg}
}

// ---------------------------------------------------------------------------
// init_worktree / cleanup_worktree
// ---------------------------------------------------------------------------

// handleInitWorktree opens a tool session pinned to a
// (workspaceID, taskID, realPath) tuple. The daemon:
//
//  1. Validates local_path (absolute, not blacklisted, exists, is a
//     directory, r/w). Mirrors validateLocalPath's checks — same
//     rules, same blacklist — so a request the daemon accepts here
//     is one the CLI runtime would also accept.
//  2. Acquires the per-path mutex via localPathLocks.Acquire. While
//     the session is live, no other task on this daemon can claim
//     the same directory.
//  3. Walks the working tree to compute the init snapshot. The
//     server's Worktree wrapper uses this list to label each
//     write_file call as "created" vs "modified" and to compute the
//     "deleted" bucket on cleanup.
//
// The session_id comes from the request payload (server-chosen UUID
// the server will reuse on retries). The daemon stores the per-task
// state under that key.
//
// On success the response carries the snapshot list as
//   {"initial": ["rel/path/1", "rel/path/2", ...]}
// sorted lexicographically for stable test output.
func (r *llmRPCDispatcher) handleInitWorktree(
	ctx context.Context,
	d *Daemon,
	_ *llmSession,
	taskID string,
	sessionID string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		LocalPath string `json:"local_path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "init_worktree: parse args: "+err.Error())
	}
	if strings.TrimSpace(a.LocalPath) == "" {
		return nil, toolErr("bad_args", "init_worktree: local_path is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, toolErr("bad_args", "init_worktree: session_id is required")
	}
	absPath, err := normalizeLocalPath(a.LocalPath)
	if err != nil {
		return nil, toolErr("bad_args", err.Error())
	}
	if err := validateLocalPath(absPath); err != nil {
		return nil, toolErr("invalid_path", err.Error())
	}
	realPath, _ := resolveRealPath(absPath)

	// Acquire the per-path mutex. Use a daemon-root context so a
	// cancelled caller ctx (e.g. WS teardown) doesn't drop the
	// lock mid-tool-session. The acquire blocks until either we
	// take the lock or the root ctx is cancelled.
	acquireCtx, cancel := context.WithTimeout(r.d.recoveryContext(), 30*time.Second)
	defer cancel()
	release, err := r.d.localPathLocks.Acquire(acquireCtx, realPath, taskID, nil)
	if err != nil {
		return nil, toolErr("lock_failed", "init_worktree: acquire path lock: "+err.Error())
	}
	// Take the snapshot. Walk the tree (skipping .git) and
	// record relative path → {size, mtime}. The list returned
	// to the server is just the sorted paths; the server
	// reconstructs size/mtime as zero values on its side (the
	// in-process tracking wins the modified-vs-created tie
	// without needing them).
	snapshot := walkSnapshot(absPath)
	paths := make([]string, 0, len(snapshot))
	for p := range snapshot {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	sess := &llmSession{
		sessionID:   sessionID,
		workspaceID: "",
		taskID:      taskID,
		realPath:    realPath,
		rootDir:     absPath,
		initial:     snapshot,
		release:     release,
		createdAt:   time.Now(),
	}
	r.sessions.Store(sessionID, sess)
	if d.logger != nil {
		d.logger.Info("llmtools: session opened",
			"session_id", sessionID,
			"path", absPath,
			"initial_files", len(paths),
		)
	}
	return json.Marshal(map[string]any{"initial": paths})
}

// handleCleanupWorktree closes the session: releases the path mutex
// and drops the session map entry. Safe to call multiple times — the
// release func is idempotent and the second session.Delete is a
// no-op.
//
// Returns the deleted-file list computed from a re-snapshot of the
// working tree. The server merges this into its "deleted" bucket; the
// model never sees this directly.
func (r *llmRPCDispatcher) handleCleanupWorktree(
	_ context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	_ json.RawMessage,
) (json.RawMessage, error) {
	if sess == nil {
		return nil, toolErr("session_not_found", "cleanup_worktree: no active session")
	}
	now := walkSnapshot(sess.rootDir)
	var deleted []string
	for p := range sess.initial {
		if _, ok := now[p]; ok {
			continue
		}
		deleted = append(deleted, p)
	}
	sort.Strings(deleted)
	if sess.release != nil {
		sess.release()
	}
	r.sessions.Delete(sess.sessionID)
	result, _ := json.Marshal(map[string]any{"deleted": deleted})
	return result, nil
}

// ---------------------------------------------------------------------------
// File / shell / git handlers
// ---------------------------------------------------------------------------

// handleReadFile reads a file inside the session root, capped at
// max_bytes. The path is sanitised by resolveSessionPath so the
// model can never read outside the sandbox.
func (r *llmRPCDispatcher) handleReadFile(
	_ context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "read_file: parse args: "+err.Error())
	}
	if a.Path == "" {
		return nil, toolErr("bad_args", "read_file: path is required")
	}
	if a.MaxBytes <= 0 {
		a.MaxBytes = 32 << 10
	}
	abs, err := resolveSessionPath(sess, a.Path)
	if err != nil {
		return nil, toolErr("path_escape", err.Error())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, toolErr("not_found", "read_file: "+err.Error())
		}
		return nil, toolErr("io_error", "read_file: "+err.Error())
	}
	if a.MaxBytes > 0 && len(data) > a.MaxBytes {
		data = data[:a.MaxBytes]
	}
	return json.Marshal(map[string]any{"content": string(data)})
}

// handleWriteFile creates or overwrites a file. The pre-existing
// flag is computed by stat'ing the path BEFORE the write so the
// server's Worktree can record the change as "created" (didn't
// exist) or "modified" (did exist).
func (r *llmRPCDispatcher) handleWriteFile(
	_ context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "write_file: parse args: "+err.Error())
	}
	if a.Path == "" {
		return nil, toolErr("bad_args", "write_file: path is required")
	}
	abs, err := resolveSessionPath(sess, a.Path)
	if err != nil {
		return nil, toolErr("path_escape", err.Error())
	}
	// Reject writes into .git/ — same rule as the server-side
	// writeFileOp. The model can commit via git_commit, not by
	// hand-editing the git internals.
	rel, _ := filepath.Rel(sess.rootDir, abs)
	if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return nil, toolErr("forbidden", "write_file: refusing to write inside .git/")
	}
	preExisting := false
	if _, statErr := os.Stat(abs); statErr == nil {
		preExisting = true
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, toolErr("io_error", "write_file: mkdir: "+err.Error())
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return nil, toolErr("io_error", "write_file: "+err.Error())
	}
	return json.Marshal(map[string]any{"pre_existing": preExisting})
}

// handleListDir returns a flattened listing of path inside the
// session root, one entry per line, with a trailing '/' for
// directories. Recursion is bounded to 4 levels so the model can
// skim the tree shape without flooding the response.
func (r *llmRPCDispatcher) handleListDir(
	_ context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "list_dir: parse args: "+err.Error())
	}
	if a.Path == "" {
		a.Path = "."
	}
	abs, err := resolveSessionPath(sess, a.Path)
	if err != nil {
		return nil, toolErr("path_escape", err.Error())
	}
	var b strings.Builder
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(abs, p)
		if rel == ".git" {
			return fs.SkipDir
		}
		if rel == "" {
			return nil
		}
		if strings.Count(rel, string(filepath.Separator)) >= 4 {
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		if d.IsDir() {
			b.WriteString(rel)
			b.WriteString("/\n")
		} else {
			b.WriteString(rel)
			b.WriteString("\n")
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, toolErr("not_found", "list_dir: "+err.Error())
		}
		return nil, toolErr("io_error", "list_dir: "+err.Error())
	}
	return json.Marshal(map[string]any{"entries": b.String()})
}

// handleRunShell runs a shell command inside the session's rootDir
// with the supplied timeout. Mirrors the server's runShellLocal:
// stdout + stderr are captured, output is truncated at 8 KiB to
// keep model contexts small, exit != 0 becomes a tool error.
func (r *llmRPCDispatcher) handleRunShell(
	ctx context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "run_shell: parse args: "+err.Error())
	}
	if strings.TrimSpace(a.Command) == "" {
		return nil, toolErr("bad_args", "run_shell: command is required")
	}
	timeout := 30 * time.Second
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)
	cmd.Dir = sess.rootDir
	// GIT_TERMINAL_PROMPT=0 keeps `git` from blocking on auth
	// prompts — the model has no stdin to type into and the
	// session is supposed to operate against the user's own
	// configured credentials. Mirrors the server-side env
	// injection.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var sb strings.Builder
	if out.Len() > 0 {
		sb.Write(out.Bytes())
	}
	if errOut.Len() > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n--- stderr ---\n")
		}
		sb.Write(errOut.Bytes())
	}
	if err != nil {
		const cap = 8 << 10
		if sb.Len() > cap {
			sb.Reset()
			sb.WriteString("[output truncated at 8 KiB; re-invoke with a smaller command]\n")
		}
		return nil, toolErr("shell_failed", fmt.Sprintf("run_shell: exit %v\n%s", err, sb.String()))
	}
	const cap = 8 << 10
	text := sb.String()
	if len(text) > cap {
		text = text[:cap] + "\n[output truncated at 8 KiB]"
	}
	if text == "" {
		text = "(no output)"
	}
	return json.Marshal(map[string]any{"output": text})
}

// handleGitStatus / GitDiff / GitCommit wrap the corresponding
// `git` invocations. They share the session's rootDir as the git
// cwd; the local-directory project is a real git working tree (or
// a plain non-git dir; in that case the commands return whatever
// error `git` raises and the model surfaces it as a tool error).
func (r *llmRPCDispatcher) handleGitStatus(
	ctx context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	_ json.RawMessage,
) (json.RawMessage, error) {
	out, err := gitOutputIn(ctx, sess.rootDir, "status", "--short")
	if err != nil {
		return nil, toolErr("git_error", "git_status: "+err.Error())
	}
	return json.Marshal(map[string]any{"output": out})
}

func (r *llmRPCDispatcher) handleGitDiff(
	ctx context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Staged bool `json:"staged"`
	}
	_ = json.Unmarshal(args, &a)
	var out string
	var err error
	if a.Staged {
		out, err = gitOutputIn(ctx, sess.rootDir, "diff", "--cached")
	} else {
		out, err = gitOutputIn(ctx, sess.rootDir, "diff")
	}
	if err != nil {
		return nil, toolErr("git_error", "git_diff: "+err.Error())
	}
	return json.Marshal(map[string]any{"output": out})
}

func (r *llmRPCDispatcher) handleGitCommit(
	ctx context.Context,
	_ *Daemon,
	sess *llmSession,
	_ string,
	_ string,
	args json.RawMessage,
) (json.RawMessage, error) {
	var a struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, toolErr("bad_args", "git_commit: parse args: "+err.Error())
	}
	if strings.TrimSpace(a.Message) == "" {
		return nil, toolErr("bad_args", "git_commit: message is required")
	}
	// Stage everything. Mirrors the server's localBackend
	// behaviour: the model picks commit timing, not files.
	if err := gitRunIn(ctx, sess.rootDir, "add", "-A"); err != nil {
		return nil, toolErr("git_error", "git_commit: git add: "+err.Error())
	}
	// Refuse to create empty commits.
	staged, err := gitOutputIn(ctx, sess.rootDir, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, toolErr("git_error", "git_commit: staged check: "+err.Error())
	}
	if strings.TrimSpace(staged) == "" {
		return nil, toolErr("nothing_to_commit", "git_commit: nothing to commit (workdir is clean)")
	}
	if err := gitRunIn(ctx, sess.rootDir, "commit", "-m", a.Message, "--no-verify"); err != nil {
		return nil, toolErr("git_error", "git_commit: git commit: "+err.Error())
	}
	sha, err := gitOutputIn(ctx, sess.rootDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, toolErr("git_error", "git_commit: rev-parse: "+err.Error())
	}
	return json.Marshal(map[string]any{"sha": strings.TrimSpace(sha)})
}

// ---------------------------------------------------------------------------
// path resolution + git helpers (shared by handlers)
// ---------------------------------------------------------------------------

// resolveSessionPath maps a model-supplied path to an absolute path
// inside the session root, rejecting anything that escapes. Mirrors
// the server-side localBackend.resolveLocalPath: empty path is
// rejected, "." is the root, absolute paths are taken verbatim, and
// any final path whose relative-from-root starts with ".." is
// refused.
func resolveSessionPath(sess *llmSession, p string) (string, error) {
	if sess == nil {
		return "", errors.New("no active session")
	}
	cleaned := filepath.Clean(p)
	var abs string
	if filepath.IsAbs(cleaned) {
		abs = cleaned
	} else {
		abs = filepath.Join(sess.rootDir, cleaned)
	}
	rel, err := filepath.Rel(sess.rootDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path %q escapes session root", p)
	}
	return abs, nil
}

// walkSnapshot builds a {relativePath → {size, mtime}} map by
// walking the directory tree at root, skipping .git. Mirrors the
// server-side (lb).snapshot — same depth-unbounded walk, same skip
// rule, same return type — so the wire shape is symmetric.
func walkSnapshot(root string) map[string]fileSnapshot {
	out := map[string]fileSnapshot{}
	if root == "" {
		return out
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return fs.SkipDir
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = fileSnapshot{size: info.Size(), mtime: info.ModTime()}
		return nil
	})
	return out
}

// gitRunIn / gitOutputIn are the daemon-side analogues of the
// server's gitRun / gitOutput. They pin the git cwd to the supplied
// dir (so the model never has to think about absolute paths inside
// run_shell) and set GIT_TERMINAL_PROMPT=0.
func gitRunIn(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutputIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// Unused — keeps the slog import live for future structured log
// lines on the dispatcher without churning the import block when
// handlers get added.
var _ = slog.Default
