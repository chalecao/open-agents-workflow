package llmexec

import (
	"bytes"
	"context"
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

	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/repocache"
)

// ---------------------------------------------------------------------------
// Backend interface
// ---------------------------------------------------------------------------

// Backend abstracts the file + git + shell operations the LLM
// tool loop drives. Two implementations exist:
//
//   - LocalBackend: the worktree lives on the server's
//     filesystem. This is the path taken for github_repo
//     projects (per-workspace bare cache + per-task worktree
//     — same on-disk shape as the daemon's CLI runtime uses)
//     and for tasks with no project resources at all (plain
//     temp dir, `git init` only).
//   - RemoteBackend: the worktree lives on a daemon's
//     filesystem (local_directory project). All file / shell
//     / git operations are forwarded to the daemon over the
//     existing task-wakeup WebSocket as RPC requests. The
//     server never touches the user's local files; the
//     daemon does. The path-mutex, the snapshot for
//     changed-files diffing, and the actual exec all live
//     on the daemon.
//
// The Worktree wrapper (below) is backend-agnostic: it
// holds the changed-files tracking and the per-task audit
// data, and delegates every I/O call to the Backend. The
// tool implementations talk to *Worktree, never to *Backend
// directly, so adding a new Backend (e.g. a future
// SSH-tunneled one) requires zero changes to the tools.
type Backend interface {
	// Root returns a human-readable identifier for the
	// workdir. LocalBackend returns the absolute
	// filesystem path. RemoteBackend returns a synthetic
	// "remote://<daemon_id>/<path>" string so logs and
	// the task result show the worktree's location even
	// though the server has no real path.
	Root() string
	// ReadFile returns the contents of path (relative to
	// the workdir) up to maxBytes. Used by read_file.
	ReadFile(ctx context.Context, path string, maxBytes int) (string, error)
	// WriteFile creates or overwrites the file at path
	// with content. preExisting is true when the file
	// already existed at Init time — the Worktree uses
	// this to record the path as "modified" rather than
	// "created" in the changed-files audit.
	WriteFile(ctx context.Context, path, content string) (preExisting bool, err error)
	// ListDir returns entries under path, one per line,
	// with a trailing '/' for directories. Hidden
	// entries are included. Used by list_dir.
	ListDir(ctx context.Context, path string) (string, error)
	// RunShell runs /bin/sh -c command in the workdir
	// with the supplied timeout. Captures stdout + stderr;
	// an error indicates a non-zero exit or a timeout.
	// Used by run_shell.
	RunShell(ctx context.Context, command string, timeout time.Duration) (string, error)
	// GitStatus / GitDiff / GitCommit wrap the
	// corresponding `git` invocations. GitCommit returns
	// the short SHA so the Worktree can append a
	// CommitRecord to its audit list.
	GitStatus(ctx context.Context) (string, error)
	GitDiff(ctx context.Context, staged bool) (string, error)
	GitCommit(ctx context.Context, message string) (sha string, err error)
	// ChangedFiles returns the audit list as known to
	// the backend (computed from the init-time
	// snapshot). The Worktree uses this only for the
	// "deleted" bucket — it trusts its own in-process
	// tracking for "created" / "modified".
	ChangedFiles() (created, modified, deleted []string)
	// initialSnapshot returns the file-snapshot map the
	// backend captured at Init time. The Worktree uses
	// it to seed its own in-process tracking — paths in
	// this map are recorded as "modified" on the first
	// write, paths outside it as "created".
	initialSnapshot() map[string]fileSnapshot
	// Cleanup releases any resources the backend holds.
	// Safe to call multiple times. The worker's defer
	// is the only caller.
	Cleanup()
}

// ---------------------------------------------------------------------------
// InitOptions
// ---------------------------------------------------------------------------

// InitOptions configures a per-task Worktree. The worker
// builds the full struct in one place so the callsite stays
// obvious about which workspace / task / resource the
// worktree is for. Field selection determines the backend:
//
//   - LocalPath set → RemoteBackend rooted at the daemon's
//     local_directory path (Hub + DaemonID + RuntimeID
//     required).
//   - RepoURL set → LocalBackend backed by the
//     per-workspace bare cache + per-task worktree
//     (RepocacheParent + WorktreeParent + WorkspaceID +
//     TaskID required).
//   - otherwise → LocalBackend as a plain temp dir
//     (WorktreeParent optional; defaults to os.TempDir()).
type InitOptions struct {
	// RepocacheParent is the parent directory under
	// which the shared bare repo cache lives when
	// RepoURL is set. Layout is
	//   <RepocacheParent>/<WorkspaceID>/<bareName>.git
	// and is keyed on (workspace, repo URL). Multiple
	// tasks in the same workspace share one bare clone;
	// per-task worktrees are cheap `git worktree add`
	// operations on top.
	RepocacheParent string
	// WorktreeParent is the parent directory under
	// which the per-task worktree is created when
	// RepoURL is set. Layout is
	//   <WorktreeParent>/<WorkspaceID>/<taskID>/
	// When RepoURL is empty and LocalPath is empty,
	// the worktree lives at
	//   <WorktreeParent>/llmexec-XXX/
	// (a fresh MkdirTemp under WorktreeParent, or
	// os.TempDir() when WorktreeParent is empty).
	WorktreeParent string
	// WorkspaceID scopes the bare cache and worktree
	// paths so different workspaces can never see each
	// other's files. Required when RepoURL is set.
	WorkspaceID string
	// TaskID names the per-task worktree. Used as the
	// leaf directory name. Required when RepoURL is
	// set. Also used as the session_id the RemoteBackend
	// hands the daemon so the daemon's per-task path
	// mutex is keyed per LLM task.
	TaskID string
	// RepoURL is the remote URL to clone. Empty means
	// the worktree is a plain temp dir (no git
	// worktree, no bare cache involvement) for
	// LocalBackend. Mutually exclusive with LocalPath
	// — when LocalPath is set, RepoURL is ignored.
	RepoURL string
	// LocalPath, when set, switches the backend to
	// RemoteBackend. The value is the absolute path
	// on the daemon's filesystem that pins the
	// project. The server doesn't see this path; the
	// daemon resolves it locally against its own
	// filesystem. Hub + DaemonID + RuntimeID must
	// also be set when LocalPath is set.
	LocalPath string
	// Hub is the daemon WebSocket hub used by
	// RemoteBackend to dispatch every file / shell /
	// git op as an RPC. Required when LocalPath is
	// set. Ignored otherwise.
	Hub *daemonws.Hub
	// DaemonID identifies the daemon that owns the
	// LocalPath. Currently used only for logging
	// (the actual dispatch is by RuntimeID, which
	// is the dispatch key the Hub indexes by).
	DaemonID string
	// RuntimeID is the runtime the daemon is
	// heartbeating for, the dispatch key the Hub
	// indexes by. Required when LocalPath is set.
	RuntimeID string
	// RequestTimeout caps each individual RPC call
	// (read_file, write_file, run_shell, …). Defaults
	// to 60 seconds. The whole turn still has its
	// own turn timeout in the worker; this is the
	// per-RPC ceiling, not the per-turn ceiling.
	RequestTimeout time.Duration
	// Logger is used for the cache clone / fetch /
	// worktree-add progress lines. Defaults to
	// slog.Default() when nil so tests don't have
	// to wire one.
	Logger *slog.Logger
}

// ---------------------------------------------------------------------------
// Worktree wrapper
// ---------------------------------------------------------------------------

// fileSnapshot is the small piece of fs metadata we record
// per path at init time. mtime is captured so ChangedFiles
// can tell "file was rewritten" from "file was touched but
// content is identical" (cheap-to-detect false positives
// — good enough for the audit list; the actual diff is
// reported in the task result).
type fileSnapshot struct {
	size  int64
	mtime time.Time
}

// CommitRecord is a single git commit the model produced.
// The WireFormat stays stable so the task result doesn't
// need to special-case remote vs local runs.
type CommitRecord struct {
	SHA        string    `json:"sha"`
	Message    string    `json:"message"`
	AuthoredAt time.Time `json:"authored_at"`
}

// Worktree is a per-task wrapper around a Backend. The LLM
// tool loop talks to *Worktree; Worktree owns the
// changed-files tracking and the per-task audit data, and
// delegates every read/write/shell/git call to the
// Backend.
//
// Splitting "state + tracking" (Worktree) from "I/O"
// (Backend) keeps the tool implementations short and
// backend-agnostic: they never need to special-case "did
// this come from the local disk or from an RPC".
type Worktree struct {
	backend Backend

	mu         sync.Mutex
	initial    map[string]fileSnapshot
	created    map[string]struct{}
	modified   map[string]struct{}
	deleted    map[string]struct{}
	commits    []CommitRecord
	shellCalls int
}

// Init prepares the per-task worktree. Behaviour depends
// on the resource type, computed from opts:
//
//   - opts.LocalPath != "" → RemoteBackend rooted at the
//     daemon's local_directory path. The daemon acquires
//     its path mutex, takes a snapshot, and returns the
//     initial file list.
//   - opts.RepoURL != "" → LocalBackend backed by the
//     per-workspace bare cache + per-task worktree, the
//     same on-disk shape the daemon's CLI runtime uses.
//   - otherwise → LocalBackend as a plain temp dir (no
//     git worktree, no bare cache) with `git init` so
//     the git_* tools still work.
//
// The init path is responsible for taking the snapshot
// the changed-files audit diffs against, regardless of
// backend.
func (w *Worktree) Init(opts InitOptions) error {
	w.created = map[string]struct{}{}
	w.modified = map[string]struct{}{}
	w.deleted = map[string]struct{}{}
	w.initial = map[string]fileSnapshot{}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	switch {
	case opts.LocalPath != "":
		be, err := newRemoteBackend(remoteBackendInit{
			Hub:            opts.Hub,
			DaemonID:       opts.DaemonID,
			RuntimeID:      opts.RuntimeID,
			WorkspaceID:    opts.WorkspaceID,
			TaskID:         opts.TaskID,
			LocalPath:      opts.LocalPath,
			RequestTimeout: opts.RequestTimeout,
			Logger:         opts.Logger,
		})
		if err != nil {
			return err
		}
		w.backend = be
		w.initial = be.initialSnapshot()
		return nil
	case opts.RepoURL != "":
		be, err := newLocalBackendFromRepo(localBackendInit{
			RepocacheParent: opts.RepocacheParent,
			WorktreeParent:  opts.WorktreeParent,
			WorkspaceID:     opts.WorkspaceID,
			TaskID:          opts.TaskID,
			RepoURL:         opts.RepoURL,
			Logger:          opts.Logger,
		})
		if err != nil {
			return err
		}
		w.backend = be
		w.initial = be.initialSnapshot()
		return nil
	default:
		be, err := newLocalBackendEmpty(localBackendInit{
			WorktreeParent: opts.WorktreeParent,
			Logger:          opts.Logger,
		})
		if err != nil {
			return err
		}
		w.backend = be
		w.initial = be.initialSnapshot()
		return nil
	}
}

// Root returns the backend's identifier. For LocalBackend
// this is the absolute path; for RemoteBackend this is a
// "remote://..." synthetic string. The worker passes the
// value through unchanged into tool_audit, so downstream
// consumers can see at a glance whether a given task ran
// on the server's disk or on a remote daemon.
func (w *Worktree) Root() string {
	if w.backend == nil {
		return ""
	}
	return w.backend.Root()
}

// Cleanup releases the per-task worktree. Safe to call
// multiple times; the worker's defer is the only caller.
func (w *Worktree) Cleanup() {
	if w.backend == nil {
		return
	}
	w.backend.Cleanup()
	w.backend = nil
}

// ChangedFiles is the audit list of every model-induced
// change in the workdir, split into three disjoint
// buckets. We prefer the Worktree's own tracking (updated
// on every write_file call) over the backend's snapshot
// diff because the in-process tracking wins ties between
// "created" and "modified" deterministically — a file the
// model created in turn 1 and then rewrote in turn 3 lands
// in "modified", the more recent state. The backend's
// view is used for the "deleted" bucket because the
// in-process tracking never observes run_shell-driven
// deletions (rm / git-clean).
//
// Returns paths relative to the workdir root, sorted
// lexicographically for stable test output.
func (w *Worktree) ChangedFiles() (created, modified, deleted []string) {
	w.mu.Lock()
	for p := range w.modified {
		modified = append(modified, p)
	}
	for p := range w.created {
		if _, ok := w.modified[p]; ok {
			continue
		}
		created = append(created, p)
	}
	w.mu.Unlock()
	// Deleted: trust the backend's view. The
	// in-process tracking never observes
	// run_shell-driven deletions.
	if w.backend != nil {
		_, _, beDeleted := w.backend.ChangedFiles()
		deleted = append(deleted, beDeleted...)
	}
	sort.Strings(created)
	sort.Strings(modified)
	sort.Strings(deleted)
	return
}

// Commits returns a copy of the commit list. Safe to call
// concurrently with git_commit.
func (w *Worktree) Commits() []CommitRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]CommitRecord, len(w.commits))
	copy(out, w.commits)
	return out
}

// ShellCalls returns the total number of run_shell
// invocations the model has made. Cheap to compute so we
// don't cache it.
func (w *Worktree) ShellCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.shellCalls
}

// ---------------------------------------------------------------------------
// Backend-forwarding methods used by the tool implementations
// ---------------------------------------------------------------------------

// readFileOp forwards to the backend, then enforces the
// max-bytes cap. The backend already truncates at the
// wire boundary for RemoteBackend, so this is a safety net
// only.
func (w *Worktree) readFileOp(ctx context.Context, path string, maxBytes int) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 10
	}
	return w.backend.ReadFile(ctx, path, maxBytes)
}

func (w *Worktree) listDirOp(ctx context.Context, path string) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	if path == "" {
		path = "."
	}
	return w.backend.ListDir(ctx, path)
}

// writeFileOp writes content to path, then updates the
// changed-files tracking based on whether the path was
// pre-existing (the backend reports this flag).
func (w *Worktree) writeFileOp(ctx context.Context, path, content string) error {
	if w.backend == nil {
		return fmt.Errorf("workdir: not initialised")
	}
	if path == "" {
		return errors.New("write_file: path is required")
	}
	// Server-side guard: refuse obvious escapes before
	// the backend ever sees them. The backend re-checks
	// (the daemon has its own containment check; the
	// local backend uses filepath.Rel), so this is
	// defense in depth.
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("write_file: path %q escapes workdir", path)
	}
	if strings.HasPrefix(cleaned, ".git"+string(filepath.Separator)) || cleaned == ".git" {
		return errors.New("write_file: refusing to write inside .git/")
	}
	preExisting, err := w.backend.WriteFile(ctx, cleaned, content)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.recordChangeLocked(cleaned, preExisting)
	w.mu.Unlock()
	return nil
}

// recordChangeLocked is the changed-files tracking core.
// "modified" wins over "created" — a file the model
// created earlier in the run and is now rewriting is the
// more recent action, and the activity log wants to show
// that.
//
// The caller must hold w.mu.
func (w *Worktree) recordChangeLocked(rel string, preExisting bool) {
	_, wasInInitial := w.initial[rel]
	_, wasCreated := w.created[rel]
	_, wasModified := w.modified[rel]
	if preExisting || wasInInitial || wasCreated || wasModified {
		w.modified[rel] = struct{}{}
		delete(w.created, rel)
		return
	}
	w.created[rel] = struct{}{}
}

func (w *Worktree) runShellOp(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	out, err := w.backend.RunShell(ctx, command, timeout)
	w.mu.Lock()
	w.shellCalls++
	w.mu.Unlock()
	return out, err
}

func (w *Worktree) gitStatusOp(ctx context.Context) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	return w.backend.GitStatus(ctx)
}

func (w *Worktree) gitDiffOp(ctx context.Context, staged bool) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	return w.backend.GitDiff(ctx, staged)
}

func (w *Worktree) gitCommitOp(ctx context.Context, message string) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	sha, err := w.backend.GitCommit(ctx, message)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.commits = append(w.commits, CommitRecord{
		SHA:        sha,
		Message:    message,
		AuthoredAt: time.Now().UTC(),
	})
	w.mu.Unlock()
	return sha, nil
}

// resolveInWorktree is a server-side path check used by
// the tools (write_file's `.git/` guard, etc.). The
// backend re-validates on its side, so this is defense in
// depth, not the only check.
func (w *Worktree) resolveInWorktree(p string) (string, error) {
	if w.backend == nil {
		return "", fmt.Errorf("workdir: not initialised")
	}
	cleaned := filepath.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir: path %q escapes workdir", p)
	}
	return cleaned, nil
}

// snapshot walks the workdir and records size+mtime for
// every regular file. Used by the local backend's
// constructor to seed `initial`. The `.git` dir is skipped
// (it grows on every commit and shouldn't show up in the
// "what changed" list).
func (lb *localBackend) snapshot() map[string]fileSnapshot {
	out := map[string]fileSnapshot{}
	if lb.root == "" {
		return out
	}
	filepath.WalkDir(lb.root, func(path string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(lb.root, path)
		out[rel] = fileSnapshot{size: info.Size(), mtime: info.ModTime()}
		return nil
	})
	return out
}

// initialSnapshot is the Backend-interface implementation
// that returns the init-time snapshot. The Worktree
// wrapper seeds its own tracking with this map.
func (lb *localBackend) initialSnapshot() map[string]fileSnapshot {
	return lb.initial
}

// git helpers — used by LocalBackend and the local tools
// directly. RemoteBackend doesn't call these; it
// forwards git ops as RPCs.

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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

// runShellLocal is the LocalBackend's RunShell
// implementation. Lives here (rather than in local.go)
// because it shares the truncation + capture logic with
// the original run_shell tool.
func runShellLocal(ctx context.Context, dir, command string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	cmd.Dir = dir
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
		return "", fmt.Errorf("run_shell: exit %v\n%s", err, sb.String())
	}
	const cap = 8 << 10
	if sb.Len() > cap {
		return sb.String()[:cap] + "\n[output truncated at 8 KiB]", nil
	}
	if sb.Len() == 0 {
		return "(no output)", nil
	}
	return sb.String(), nil
}

// keep repocache import — used by the LocalBackend
var _ = repocache.New
