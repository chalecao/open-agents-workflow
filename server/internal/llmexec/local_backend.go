package llmexec

import (
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
	"time"

	"github.com/multica-ai/multica/server/internal/repocache"
)

// localBackendInit is the constructor input for both
// localBackend constructors. WorktreeParent / RepocacheParent
// are optional / required per constructor (see below); the
// rest are common to both.
type localBackendInit struct {
	RepocacheParent string
	WorktreeParent  string
	WorkspaceID     string
	TaskID          string
	RepoURL         string
	Logger          *slog.Logger
}

// localBackend implements Backend for worktrees that live on
// the server's filesystem. The two constructors cover the two
// flavours the worker needs:
//
//   - newLocalBackendFromRepo: github_repo / git projects.
//     Allocates a per-task worktree on top of a per-workspace
//     bare cache (see internal/repocache). Matches the
//     daemon's CLI-runtime worktree semantics exactly —
//     same on-disk shape, same `git worktree add` plumbing,
//     same concurrent-task locking, same cleanup via
//     `git worktree remove` + `prune`. An LLM task and a
//     CLI task in the same workspace share one bare clone;
//     only the per-task worktree is newly allocated.
//
//   - newLocalBackendEmpty: tasks with no project resources
//     (chat, autopilot without project, etc.). Just a fresh
//     MkdirTemp under WorktreeParent (or os.TempDir() when
//     WorktreeParent is empty), with `git init` so the
//     git_* tools still work.
type localBackend struct {
	root     string
	barePath string
	branch   string
	logger   *slog.Logger

	// initial is the snapshot taken at Init time. The
	// Worktree wrapper takes its own copy via
	// (lb).snapshot() for created/modified tracking, but
	// the local backend also needs the snapshot for
	// ChangedFiles' "deleted" computation — the
	// in-process tracking never observes run_shell-driven
	// deletions, so the truth lives here.
	initial map[string]fileSnapshot
}

func newLocalBackendFromRepo(init localBackendInit) (*localBackend, error) {
	if init.RepocacheParent == "" || init.WorktreeParent == "" {
		return nil, errors.New("localBackend: RepocacheParent and WorktreeParent are required when RepoURL is set")
	}
	if init.WorkspaceID == "" || init.TaskID == "" {
		return nil, errors.New("localBackend: WorkspaceID and TaskID are required when RepoURL is set")
	}
	if init.Logger == nil {
		init.Logger = slog.Default()
	}
	lb := &localBackend{logger: init.Logger}
	cache := repocache.New(init.RepocacheParent, init.Logger)
	// Sync clones the bare if missing, fetches latest if
	// present. After Sync, Lookup is guaranteed to find
	// the bare path (modulo a clone failure, which is
	// non-fatal per the repocache contract — the worker
	// will get a fresh clone on the next attempt).
	if err := cache.Sync(init.WorkspaceID, []repocache.RepoInfo{{URL: init.RepoURL}}); err != nil {
		// Sync returns the first clone/fetch error but
		// continues for the rest. We still try to use
		// what it gave us.
		init.Logger.Warn("localBackend: bare cache sync reported error; continuing",
			"url", init.RepoURL, "error", err)
	}
	barePath := cache.Lookup(init.WorkspaceID, init.RepoURL)
	if barePath == "" {
		return nil, fmt.Errorf("localBackend: bare cache empty for %s (workspace %s) after sync", init.RepoURL, init.WorkspaceID)
	}
	// Per-task worktree lives at
	//   <WorktreeParent>/<WorkspaceID>/<TaskID>/
	// so the per-workspace subdir keeps different
	// workspaces' tasks visually separated when an
	// operator inspects the disk.
	wtDir := filepath.Join(init.WorktreeParent, init.WorkspaceID, init.TaskID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return nil, fmt.Errorf("localBackend: mkdir parent: %w", err)
	}
	res, err := cache.CreateWorktree(repocache.WorktreeParams{
		WorkspaceID: init.WorkspaceID,
		RepoURL:     init.RepoURL,
		WorkDir:     wtDir,
		Ref:         "", // default branch from the bare's remote-tracking
		AgentName:   "llmexec",
		TaskID:      init.TaskID,
		// CoAuthoredByEnabled deliberately false — the
		// LLM doesn't need a "Co-authored-by: <agent>"
		// trailer in its commits.
	})
	if err != nil {
		return nil, fmt.Errorf("localBackend: create worktree: %w", err)
	}
	lb.root = res.Path
	lb.barePath = barePath
	lb.branch = res.BranchName
	lb.initial = lb.snapshot()
	return lb, nil
}

func newLocalBackendEmpty(init localBackendInit) (*localBackend, error) {
	if init.Logger == nil {
		init.Logger = slog.Default()
	}
	parent := init.WorktreeParent
	if parent == "" {
		parent = os.TempDir()
	}
	root, err := os.MkdirTemp(parent, "llmexec-*")
	if err != nil {
		return nil, fmt.Errorf("localBackend: mkdir: %w", err)
	}
	// git init in a fresh workdir so the git_* tools
	// (status / diff / commit) have a real repo to work
	// with. `git init -q -b main` keeps the output
	// clean (no "Initialized empty Git repository" line)
	// and pins the default branch to main.
	if err := gitRun(root, "init", "-q", "-b", "main"); err != nil {
		// Non-fatal — without git the model can still
		// read / write files; the git_* tools will
		// fail with a clear error when invoked.
		init.Logger.Warn("localBackend: git init failed; git_* tools will be unavailable", "error", err)
	}
	// Committer identity for any commits the model
	// makes. The email prefix makes it obvious in
	// `git log` that an LLM worker produced the
	// commit, not a human.
	_ = gitRun(root, "config", "user.email", "llmexec@multica.local")
	_ = gitRun(root, "config", "user.name", "Multica LLM Worker")
	lb := &localBackend{root: root, logger: init.Logger}
	lb.initial = lb.snapshot()
	return lb, nil
}

// ---------------------------------------------------------------------------
// Backend interface implementation
// ---------------------------------------------------------------------------

func (lb *localBackend) Root() string { return lb.root }

// ReadFile reads path (relative to the workdir) up to maxBytes.
// The workdir root is implicit; the path is sanitised by
// resolveLocalPath which also enforces the `.git/` block.
func (lb *localBackend) ReadFile(ctx context.Context, path string, maxBytes int) (string, error) {
	if lb.root == "" {
		return "", errors.New("localBackend: not initialised")
	}
	abs, err := lb.resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[:maxBytes]
	}
	return string(data), nil
}

// WriteFile writes content to path. The pre-existing flag is
// computed by stat'ing the path; that's the source of truth
// for the Worktree's changed-files tracking. Parent dirs are
// created automatically.
func (lb *localBackend) WriteFile(ctx context.Context, path, content string) (bool, error) {
	if lb.root == "" {
		return false, errors.New("localBackend: not initialised")
	}
	abs, err := lb.resolveLocalPath(path)
	if err != nil {
		return false, err
	}
	preExisting := false
	if _, statErr := os.Stat(abs); statErr == nil {
		preExisting = true
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return preExisting, err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return preExisting, err
	}
	return preExisting, nil
}

// ListDir returns a flattened listing of the directory at
// path. We cap recursion at 4 levels so the model's tool
// output stays bounded — the operator can always use
// run_shell + find for deeper trees. The worktree root
// is implicit.
func (lb *localBackend) ListDir(ctx context.Context, path string) (string, error) {
	if lb.root == "" {
		return "", errors.New("localBackend: not initialised")
	}
	abs, err := lb.resolveLocalPath(path)
	if err != nil {
		return "", err
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
		// Bound recursion: count path separators in
		// the relative path. Cheap approximation; the
		// model's only meant to skim the tree shape,
		// not stream it.
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
		return "", err
	}
	return b.String(), nil
}

// RunShell delegates to the package-level runShellLocal so
// both the Worktree wrapper and any future code that holds a
// Backend can call into the same exec plumbing.
func (lb *localBackend) RunShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if lb.root == "" {
		return "", errors.New("localBackend: not initialised")
	}
	return runShellLocal(ctx, lb.root, command, timeout)
}

func (lb *localBackend) GitStatus(ctx context.Context) (string, error) {
	return gitOutput(lb.root, "status", "--short")
}

func (lb *localBackend) GitDiff(ctx context.Context, staged bool) (string, error) {
	if staged {
		return gitOutput(lb.root, "diff", "--cached")
	}
	return gitOutput(lb.root, "diff")
}

func (lb *localBackend) GitCommit(ctx context.Context, message string) (string, error) {
	// Stage everything. The model can use git_status to
	// decide whether to commit at all; once it does, it
	// commits the full set. This matches the daemon's
	// `multica commit` UX where users don't pick
	// individual files.
	if err := gitRun(lb.root, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}
	// Fail the call (instead of producing an empty
	// commit) if there is nothing staged — an empty
	// commit would still create a SHA but bloat the
	// history with no-ops.
	if out, err := gitOutput(lb.root, "diff", "--cached", "--name-only"); err != nil {
		return "", fmt.Errorf("git staged check: %w", err)
	} else if strings.TrimSpace(out) == "" {
		return "", errors.New("nothing to commit (workdir is clean)")
	}
	if err := gitRun(lb.root, "commit", "-m", message, "--no-verify"); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	sha, err := gitOutput(lb.root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// ChangedFiles returns the audit list computed from a
// re-snapshot of the workdir diffed against the init-time
// snapshot. The Worktree wrapper prefers its own in-process
// tracking for created/modified; this method is the
// authoritative source for "deleted" (rm / git-clean via
// run_shell) since the in-process tracking never observes
// run_shell-driven deletions.
//
// Created/modified are left empty here; the Worktree
// combines this with its own tracking.
func (lb *localBackend) ChangedFiles() (created, modified, deleted []string) {
	now := lb.snapshot()
	for p := range lb.initial {
		if _, ok := now[p]; ok {
			continue
		}
		deleted = append(deleted, p)
	}
	sort.Strings(deleted)
	return nil, nil, deleted
}

// Cleanup releases the per-task worktree. Two paths:
//
//   - With bare cache: `git worktree remove` from the
//     bare tears down the working dir AND unlinks the
//     worktree entry from the bare's .git/worktrees/
//     registry. `git worktree prune` cleans up the
//     bare's worktree admin dir for any prior failed
//     cleanup. The bare itself is intentionally left in
//     place — the next task in the same workspace
//     reuses it.
//   - Without bare cache: just `os.RemoveAll`. The
//     branch is local to the worktree (it was created
//     by `git init` + the first commit), so removing
//     the dir takes the branch with it.
//
// Safe to call multiple times. The worker always calls
// this from a defer. Errors are swallowed after logging
// because there's nothing useful the worker can do at
// task end if cleanup itself fails; a stale worktree
// is recovered on the next pass when prune runs.
func (lb *localBackend) Cleanup() {
	if lb.root == "" {
		return
	}
	if lb.barePath != "" {
		// worktree remove has to run from inside the
		// worktree (or with `-C`); the bare path
		// isn't enough on its own because git needs
		// the worktree's .git file to know which one
		// to remove.
		cmd := exec.Command("git", "-C", lb.root, "worktree", "remove", "--force", lb.root)
		if out, err := cmd.CombinedOutput(); err != nil {
			if lb.logger != nil {
				lb.logger.Warn("localBackend: git worktree remove failed; falling back to os.RemoveAll",
					"path", lb.root, "error", err, "output", strings.TrimSpace(string(out)))
			}
		}
		// prune runs in the bare so the worktree
		// admin dir (refs/heads/* refs, etc.)
		// doesn't leak entries for removed
		// worktrees.
		pruneCmd := exec.Command("git", "-C", lb.barePath, "worktree", "prune")
		if out, err := pruneCmd.CombinedOutput(); err != nil && lb.logger != nil {
			lb.logger.Warn("localBackend: git worktree prune failed", "bare", lb.barePath, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
	os.RemoveAll(lb.root)
	lb.root = ""
	lb.barePath = ""
}

// resolveLocalPath maps a model-supplied path (relative or
// absolute) to an absolute path inside the workdir, rejecting
// anything that would escape the root. Returns an error if
// the path resolves outside the workdir — the model can
// never reach /etc/passwd or the host filesystem, only the
// files inside its sandbox.
func (lb *localBackend) resolveLocalPath(p string) (string, error) {
	if lb.root == "" {
		return "", errors.New("localBackend: not initialised")
	}
	cleaned := filepath.Clean(p)
	var abs string
	if filepath.IsAbs(cleaned) {
		abs = cleaned
	} else {
		abs = filepath.Join(lb.root, cleaned)
	}
	rel, err := filepath.Rel(lb.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("localBackend: path %q escapes workdir", p)
	}
	return abs, nil
}

// sort.Strings is sufficient for the test helpers in this
// file; keeping the import live avoids the "imported and
// not used" churn when the file gets trimmed down.
var _ = sort.Strings
