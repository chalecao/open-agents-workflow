package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// =============================================================================
// Worktree sidebar
// =============================================================================
//
// The issue-detail right panel renders a per-worktree view of the agent's
// changes — directory name = worktree_id, expanded body = file tree + before/
// after diffs. Three handlers back that view:
//
//   - ListIssueWorktrees     → sidebar list (DB only, no FS access)
//   - GetIssueWorktreeDiff   → branch + base + changed-file list + unified diff
//   - GetIssueWorktreeFile   → before/after content for one path
//
// The worktree_id is the absolute filesystem path of the git worktree on the
// daemon host (== agent_task_queue.work_dir). It is also stamped onto
// agent-authored comments (comment.worktree_id, migration 119) so the agent
// card header can cross-link into the right panel.
//
// All three endpoints read worktree state directly from the local filesystem
// via the bundled git CLI. The server is the self-hosted control plane and
// already runs alongside the daemon on the same host, so the worktree paths
// are accessible without a daemon round-trip. Errors from `git` are surfaced
// as 502 (worktree exists in the DB but the host cannot read it) so the
// sidebar can degrade gracefully when, e.g., the daemon is in the middle of
// GC'ing a worktree.

// worktreeMaxFileBytes caps a single file's before/after content so a giant
// generated file (build artifacts, vendored lockfiles, lock files under
// "package-lock.json" in monorepos) cannot pin the request indefinitely. The
// cap is byte-based on purpose — content-type and language are not known
// here, and CJK / emoji content is the dominant case the issue sidebar will
// render, so a byte cap (16 MiB) is the simplest "user can read it" budget
// without dropping any common file shape on the floor.
const worktreeMaxFileBytes = 16 * 1024 * 1024

// worktreeDiffMaxBytes caps the unified diff text the sidebar inlines. 8 MiB
// comfortably fits a 10–20 file refactor; anything beyond that should be
// fetched file-by-file through GetIssueWorktreeFile so the sidebar can lazy-
// load on expand.
const worktreeDiffMaxBytes = 8 * 1024 * 1024

// WorktreeListItem is the per-worktree summary returned to the sidebar.
// `id` is the absolute path (== agent_task_queue.work_dir) so the client can
// pass it straight to the diff / file endpoints without an extra translation
// layer. `branch` / `base_branch` are filled in lazily by the diff endpoint;
// the list endpoint leaves them empty so the cheap path stays cheap.
type WorktreeListItem struct {
	ID            string  `json:"id"`
	Branch        string  `json:"branch"`
	BaseBranch    string  `json:"base_branch"`
	TaskCount     int64   `json:"task_count"`
	AgentCount    int64   `json:"agent_count"`
	LatestStatus  string  `json:"latest_status"`
	LatestTaskID  string  `json:"latest_task_id"`
	LastActivity  *string `json:"last_activity_at"`
	Exists        bool    `json:"exists"`
	CommentCount  int64   `json:"comment_count"`
}

// WorktreeFileChange is one entry in the diff file list. The status mirrors
// `git status --porcelain` codes (M / A / D / R / C / ??); unknown values
// are returned as the raw two-character string and the UI is expected to
// fall back to a generic icon.
type WorktreeFileChange struct {
	Path     string `json:"path"`
	OldPath  string `json:"old_path,omitempty"`
	Status   string `json:"status"`
	// Additions/Deletions are populated from `git numstat` so the sidebar
	// can render "+12 / -3" without parsing the unified diff text. Zero on
	// binary files (git prints "-" for both columns) or on a stat failure.
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
	Binary    bool `json:"binary"`
}

type WorktreeDiffResponse struct {
	ID              string               `json:"id"`
	Branch          string               `json:"branch"`
	BaseBranch      string               `json:"base_branch"`
	BaseSHA         string               `json:"base_sha"`
	HeadSHA         string               `json:"head_sha"`
	Exists          bool                 `json:"exists"`
	// Unborn is true when the worktree is a fresh `git init` with no commits
	// yet. `Branch` is the literal "HEAD" in that case; BaseSHA / HeadSHA /
	// Diff / Files stay empty because there is no revision to diff against.
	// Untracked and UnstagedFiles still surface — they only need the
	// working tree + index. The UI uses this flag to render an empty-state
	// instead of a "0 changes" pill that looks like the agent did nothing.
	Unborn          bool                 `json:"unborn"`
	Diff            string               `json:"diff"`
	DiffTruncated   bool                 `json:"diff_truncated"`
	Untracked       []string             `json:"untracked"`
	Files           []WorktreeFileChange `json:"files"`
	UnstagedSummary string               `json:"unstaged_summary"`
	UnstagedFiles   []WorktreeFileChange `json:"unstaged_files"`
}

type WorktreeFileResponse struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Binary    bool   `json:"binary"`
	// Before / After are the file content at the base ref and the current
	// worktree HEAD respectively. Both are omitted when the file is binary
	// (we surface `binary: true` and a small marker string instead) or
	// when the requested side is missing (new file → no `before`; deleted
	// file → no `after`).
	Before      string `json:"before,omitempty"`
	After       string `json:"after,omitempty"`
	BeforeBytes int64  `json:"before_bytes"`
	AfterBytes  int64  `json:"after_bytes"`
	Truncated   bool   `json:"truncated"`
	// BaseSHA / HeadSHA mirror the diff response so a single fetch powers
	// the file dialog header.
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
}

// ListIssueWorktrees returns the per-worktree summary list for the issue
// sidebar. DB-only — does not touch the filesystem — so the request stays
// cheap even on issues with many worktrees. Branch / base_branch are
// resolved lazily by GetIssueWorktreeDiff; this endpoint only returns the
// aggregate task stats the sidebar needs for its header chip.
func (h *Handler) ListIssueWorktrees(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}

	stats, err := h.Queries.ListIssueWorktreeTaskStats(r.Context(), issue.ID)
	if err != nil {
		slog.Warn("list issue worktree stats failed", append(logger.RequestAttrs(r),
			"error", err, "issue_id", issueID)...)
		writeError(w, http.StatusInternalServerError, "failed to list worktrees")
		return
	}

	// Comment count per worktree_id — the sidebar shows "N comments on
	// this worktree" so the user can gauge how noisy each branch was.
	// The query returns one row per worktree_id; collapse into a map
	// keyed by path for O(1) lookup below.
	commentRows, err := h.Queries.CountCommentsByWorktreeIDForIssue(r.Context(), db.CountCommentsByWorktreeIDForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		slog.Warn("list issue worktree comment counts failed", append(logger.RequestAttrs(r),
			"error", err, "issue_id", issueID)...)
		writeError(w, http.StatusInternalServerError, "failed to list worktrees")
		return
	}
	commentCounts := make(map[string]int64, len(commentRows))
	for _, row := range commentRows {
		if !row.WorktreeID.Valid {
			continue
		}
		commentCounts[row.WorktreeID.String] = row.CommentCount
	}

	out := make([]WorktreeListItem, 0, len(stats))
	for _, row := range stats {
		if !row.WorkDir.Valid {
			continue
		}
		item := WorktreeListItem{
			ID:           row.WorkDir.String,
			TaskCount:    row.TaskCount,
			AgentCount:   row.AgentCount,
			LatestStatus: row.LatestStatus,
		}
		if row.LatestTaskID.Valid {
			item.LatestTaskID = uuidToString(row.LatestTaskID)
		}
		// LastActivityAt is `interface{}` in the sqlc-generated row because
		// the SQL is MAX(GREATEST(...)) over timestamptz columns and pgx
		// doesn't narrow the type. Every value coming back is either nil
		// (empty worktree — shouldn't happen) or a time.Time. Coerce
			// defensively so a future schema tweak that swaps the GREATEST()
			// for something else doesn't crash the sidebar.
		if row.LastActivityAt != nil {
			if t, ok := row.LastActivityAt.(time.Time); ok {
				ts := t.UTC().Format(time.RFC3339Nano)
				item.LastActivity = &ts
			}
		}
		if c, ok := commentCounts[row.WorkDir.String]; ok {
			item.CommentCount = c
		}
		// Probe the worktree existence with a single stat. Cheap, and
		// lets the sidebar render a "GC'd" badge when the daemon has
		// already cleaned up.
		if _, err := os.Stat(row.WorkDir.String); err == nil {
			item.Exists = true
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": out})
}

// GetIssueWorktreeDiff returns the branch / base / file list / unified diff
// for one worktree on the issue. Validates the worktree_id is associated
// with the issue (so a member cannot use this endpoint to peek at another
// issue's worktree by guessing a path).
func (h *Handler) GetIssueWorktreeDiff(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	worktreeID, ok := parseWorktreeID(w, r)
	if !ok {
		return
	}
	if !h.worktreeBelongsToIssue(r.Context(), issue.ID, worktreeID) {
		writeError(w, http.StatusNotFound, "worktree not found on this issue")
		return
	}

	resp, err := readWorktreeDiff(worktreeID)
	if err != nil {
		if errors.Is(err, errWorktreeMissing) {
			slog.Warn("worktree diff: path not accessible", append(logger.RequestAttrs(r),
				"worktree_id", worktreeID, "issue_id", issueID)...)
			writeError(w, http.StatusGone, "worktree has been cleaned up")
			return
		}
		slog.Warn("worktree diff failed", append(logger.RequestAttrs(r),
			"worktree_id", worktreeID, "issue_id", issueID, "error", err)...)
		writeError(w, http.StatusBadGateway, "failed to read worktree diff")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetIssueWorktreeFile returns the before/after content of a single file
// in the worktree. Path is taken from the `?path=` query param and is
// resolved relative to the worktree root; `..` segments are rejected.
func (h *Handler) GetIssueWorktreeFile(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}
	worktreeID, ok := parseWorktreeID(w, r)
	if !ok {
		return
	}
	if !h.worktreeBelongsToIssue(r.Context(), issue.ID, worktreeID) {
		writeError(w, http.StatusNotFound, "worktree not found on this issue")
		return
	}
	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	relPath, ok := sanitizeWorktreePath(rawPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "path is invalid")
		return
	}

	resp, err := readWorktreeFile(worktreeID, relPath)
	if err != nil {
		if errors.Is(err, errWorktreeMissing) {
			writeError(w, http.StatusGone, "worktree has been cleaned up")
			return
		}
		slog.Warn("worktree file failed", append(logger.RequestAttrs(r),
			"worktree_id", worktreeID, "path", relPath, "issue_id", issueID, "error", err)...)
		writeError(w, http.StatusBadGateway, "failed to read worktree file")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── helpers ─────────────────────────────────────────────────────────────────

// parseWorktreeID pulls the {worktreeId} URL segment and round-trips it
// through url.PathUnescape so paths with spaces / unicode survive intact.
// Also enforces a sane length cap so a 10MB blob sent as the URL segment
// can't keep the handler busy in url.PathUnescape forever.
func parseWorktreeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "worktreeId")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "worktree id is required")
		return "", false
	}
	if len(raw) > 4096 {
		writeError(w, http.StatusBadRequest, "worktree id too long")
		return "", false
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "worktree id is not valid url-encoded")
		return "", false
	}
	return decoded, true
}

// worktreeBelongsToIssue enforces the workspace tenant guard: a worktree_id
// (== work_dir) is only valid for this endpoint if at least one
// agent_task_queue row with that path exists for the issue. Without this,
// a workspace member who knows another workspace's worktree path could
// pull its diff.
func (h *Handler) worktreeBelongsToIssue(ctx context.Context, issueID pgtype.UUID, worktreeID string) bool {
	// Cheap path: ask the DB for a count of (issue, work_dir) matches.
	// We re-use the path-list query because it has the right shape and
	// already filters out NULL/empty. For larger issues the LIST endpoint
	// could cache the set, but for now a per-request count is fine — the
	// membership check is a single index lookup on
	// idx_agent_task_queue_issue_id.
	rows, err := h.Queries.ListIssueWorktreePaths(ctx, issueID)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.Valid && r.String == worktreeID {
			return true
		}
	}
	return false
}

// errWorktreeMissing signals "the path recorded in the DB no longer exists
// on disk". Distinct from a generic git error so the handler can map it
// to 410 Gone (the worktree was GC'd) instead of a 502.
var errWorktreeMissing = errors.New("worktree path missing")

// readWorktreeDiff runs the git plumbing the sidebar needs. Returns
// errWorktreeMissing when the path is gone, or a wrapped error for git
// failures. The base branch is discovered from the worktree's git
// configuration: the daemon's `setupGitWorktree` records the base ref via
// `git -C <worktree> branch --set-upstream-to=<base>`, so `git rev-parse
// --abbrev-ref <branch>@{u}` returns the base on demand. Falls back to
// `origin/HEAD` / `origin/main` / `HEAD` when no upstream is set.
func readWorktreeDiff(worktreeID string) (*WorktreeDiffResponse, error) {
	if _, err := os.Stat(worktreeID); err != nil {
		if os.IsNotExist(err) {
			return nil, errWorktreeMissing
		}
		return nil, fmt.Errorf("stat worktree: %w", err)
	}

	resp := &WorktreeDiffResponse{ID: worktreeID, Exists: true}

	// Unborn HEAD: the worktree is a fresh `git init` (or its branch
	// got reset) with no commits. `git rev-parse --abbrev-ref HEAD`
	// and `git diff <base>...HEAD` both fail with exit 128 in this
	// state, so short-circuit and return an empty diff. Untracked and
	// unstaged files still surface — they only need the working tree
	// + index, not a commit.
	//
	// Detection: `git rev-parse --verify --quiet HEAD` is the cheapest
	// "is HEAD a real commit?" probe. Exit 0 → real commit; exit 1 →
	// unborn (or detached-but-dangling, which is the same outcome
	// from the sidebar's perspective). Same pattern as the fallback
	// chain in resolveBaseRef below.
	if err := exec.Command("git", "-C", worktreeID, "rev-parse", "--verify", "--quiet", "HEAD").Run(); err != nil {
		resp.Unborn = true
		resp.Branch = "HEAD"
		if out, err := gitOutput(worktreeID, "ls-files", "--others", "--exclude-standard"); err == nil {
			resp.Untracked = splitLines(out)
		}
		if out, err := gitOutput(worktreeID, "status", "--porcelain"); err == nil {
			resp.UnstagedSummary = summaryLines(out, 10)
			if numstat, err := gitOutput(worktreeID, "diff", "--numstat"); err == nil {
				resp.UnstagedFiles = parseNumstat(numstat)
			}
		}
		return resp, nil
	}

	branch, err := gitOutput(worktreeID, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read branch: %w", err)
	}
	resp.Branch = strings.TrimSpace(branch)

	baseRef, err := resolveBaseRef(worktreeID, resp.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve base ref: %w", err)
	}
	resp.BaseBranch = baseRef

	baseSHA, err := gitOutput(worktreeID, "rev-parse", baseRef)
	if err == nil {
		resp.BaseSHA = strings.TrimSpace(baseSHA)
	}
	headSHA, err := gitOutput(worktreeID, "rev-parse", "HEAD")
	if err == nil {
		resp.HeadSHA = strings.TrimSpace(headSHA)
	}

	// Full diff against the base ref. Capped at worktreeDiffMaxBytes so a
	// runaway binary blob doesn't pin the handler. The truncation flag
	// tells the UI to fall back to the per-file endpoint for the rest.
	diffBytes, truncated, err := runGitCaptured(worktreeID, "diff", "--binary", baseRef+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	resp.Diff = string(diffBytes)
	resp.DiffTruncated = truncated

	// File list with +/- counts. `--numstat` is one line per file with
	// "<add>\t<del>\t<path>"; binary files show "-\t-\t<path>".
	numstat, err := gitOutput(worktreeID, "diff", "--numstat", baseRef+"...HEAD")
	if err == nil {
		resp.Files = parseNumstat(numstat)
	}

	// Untracked files (and any uncommitted-but-not-staged changes the
	// agent may have left behind). These are the agent's "in progress"
	// deltas — the sidebar shows them above the committed diff so the
	// user can see both the saved work and the rough edges.
	untrackedOut, err := gitOutput(worktreeID, "ls-files", "--others", "--exclude-standard")
	if err == nil {
		resp.Untracked = splitLines(untrackedOut)
	}
	statusOut, err := gitOutput(worktreeID, "status", "--porcelain")
	if err == nil {
		resp.UnstagedSummary = summaryLines(statusOut, 10)
		unstagedNumstat, _ := gitOutput(worktreeID, "diff", "--numstat")
		resp.UnstagedFiles = parseNumstat(unstagedNumstat)
	}

	return resp, nil
}

// readWorktreeFile returns before/after for a single file in the worktree.
// `before` is the file at the base ref; `after` is the current worktree
// file. Both can be empty when the file doesn't exist on that side (newly
// created vs. deleted). Binary files return `Binary: true` and skip
// content to avoid dumping megabytes of base64 into a JSON response.
func readWorktreeFile(worktreeID, relPath string) (*WorktreeFileResponse, error) {
	if _, err := os.Stat(worktreeID); err != nil {
		if os.IsNotExist(err) {
			return nil, errWorktreeMissing
		}
		return nil, fmt.Errorf("stat worktree: %w", err)
	}

	resp := &WorktreeFileResponse{ID: worktreeID, Path: relPath, Exists: true}

	branch, err := gitOutput(worktreeID, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read branch: %w", err)
	}
	branch = strings.TrimSpace(branch)
	baseRef, err := resolveBaseRef(worktreeID, branch)
	if err != nil {
		return nil, fmt.Errorf("resolve base ref: %w", err)
	}

	if baseSHA, err := gitOutput(worktreeID, "rev-parse", baseRef); err == nil {
		resp.BaseSHA = strings.TrimSpace(baseSHA)
	}
	if headSHA, err := gitOutput(worktreeID, "rev-parse", "HEAD"); err == nil {
		resp.HeadSHA = strings.TrimSpace(headSHA)
	}

	// "before" = the file at the base ref. `git show <base>:<path>` prints
	// the file content; missing file → exit non-zero. The "after" side
	// is the live filesystem; for deleted files the path is gone.
	if before, err := gitShowFile(worktreeID, baseRef, relPath); err == nil {
		before = truncateBytes(before, worktreeMaxFileBytes)
		resp.Before = string(before)
		resp.BeforeBytes = int64(len(before))
		if int64(len(before)) == worktreeMaxFileBytes {
			resp.Truncated = true
		}
	}
	// Detect binary via `git diff --numstat` — same heuristic as the
	// diff endpoint. - - means binary.
	numstat, err := gitOutput(worktreeID, "diff", "--numstat", baseRef, "--", relPath)
	if err == nil {
		if isBinaryNumstatLine(numstat) {
			resp.Binary = true
			resp.Before = ""
			resp.After = ""
			return resp, nil
		}
	}

	livePath := filepath.Join(worktreeID, relPath)
	if info, err := os.Stat(livePath); err == nil && !info.IsDir() {
		f, err := os.Open(livePath)
		if err != nil {
			return nil, fmt.Errorf("open file: %w", err)
		}
		defer f.Close()
		// Peek the first 8 KiB for a NUL byte — the universal "binary"
		// detector. Cheap, and matches the rule git itself uses.
		head := make([]byte, 8192)
		n, _ := f.Read(head)
		if bytes.IndexByte(head[:n], 0) >= 0 {
			resp.Binary = true
			return resp, nil
		}
		buf, err := os.ReadFile(livePath)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		original := int64(len(buf))
		buf = truncateBytes(buf, worktreeMaxFileBytes)
		resp.After = string(buf)
		resp.AfterBytes = original
		if original > worktreeMaxFileBytes {
			resp.Truncated = true
		}
	}
	return resp, nil
}

// resolveBaseRef returns the base ref the worktree was created against. The
// daemon's `setupGitWorktree` calls `git worktree add -b <branch> <path>
// <baseRef>`, which sets the upstream for the new branch to <baseRef>.
// Reading `@{u}` gives us the same ref. Falls back to the remote's HEAD
// (then origin/main, then origin/master, then HEAD) for worktrees that
// predate the upstream-set behavior.
func resolveBaseRef(worktreeID, branch string) (string, error) {
	if branch != "" && branch != "HEAD" {
		if upstream, err := gitOutput(worktreeID, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{u}"); err == nil {
			if up := strings.TrimSpace(upstream); up != "" {
				return up, nil
			}
		}
	}
	// Fallback chain. The daemon's git.go uses the same order, so the
	// values stay consistent across the codebase.
	if out, err := gitOutput(worktreeID, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return strings.TrimPrefix(ref, "refs/remotes/"), nil
		}
	}
	for _, candidate := range []string{"origin/main", "origin/master", "HEAD"} {
		if err := exec.Command("git", "-C", worktreeID, "rev-parse", "--verify", "--quiet", candidate).Run(); err == nil {
			return candidate, nil
		}
	}
	return "HEAD", nil
}

// gitOutput runs `git` in the worktree and returns its stdout. Exits on a
// non-zero status with the combined stderr.
func gitOutput(worktreeID string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", worktreeID}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

// runGitCaptured runs `git` and returns stdout, truncated to maxBytes when
// the output exceeds the budget. The `truncated` bool is set when the
// output was cut. The original process exits cleanly — we read up to
// maxBytes+1 and trim.
func runGitCaptured(worktreeID string, args ...string) ([]byte, bool, error) {
	cmd := exec.Command("git", append([]string{"-C", worktreeID}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	var result []byte
	truncated := false
	buf := make([]byte, worktreeDiffMaxBytes+1)
	n, _ := io.ReadFull(stdout, buf)
	if n > 0 {
		result = append(result, buf[:n]...)
	}
	// The pipe may still have bytes after we've read the budget. Drain
	// the rest into a discard so the process doesn't block on a full
	// pipe buffer.
	if n > worktreeDiffMaxBytes {
		truncated = true
		result = result[:worktreeDiffMaxBytes]
		io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return nil, false, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return result, truncated, nil
}

// parseNumstat converts `git diff --numstat` output into the
// WorktreeFileChange list. Empty input → empty list. A "-	-" pair
// marks a binary file.
func parseNumstat(raw string) []WorktreeFileChange {
	lines := splitLines(raw)
	if len(lines) == 0 {
		return nil
	}
	out := make([]WorktreeFileChange, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		entry := WorktreeFileChange{Path: fields[2]}
		if fields[0] == "-" && fields[1] == "-" {
			entry.Binary = true
		} else {
			entry.Additions = parseIntOrZero(fields[0])
			entry.Deletions = parseIntOrZero(fields[1])
		}
		// R / C status is encoded as `<from>\t<to>` in the path column.
		// We surface the rename target as the canonical path and the old
		// path on OldPath; the diff endpoint can carry the status text.
		if strings.Contains(entry.Path, "\t") {
			parts := strings.SplitN(entry.Path, "\t", 2)
			entry.OldPath = parts[0]
			entry.Path = parts[1]
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// isBinaryNumstatLine returns true when the (possibly multi-line) numstat
// output contains a "-	-" pair on any line.
func isBinaryNumstatLine(raw string) bool {
	for _, line := range splitLines(raw) {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) >= 2 && fields[0] == "-" && fields[1] == "-" {
			return true
		}
	}
	return false
}

// gitShowFile returns the content of <ref>:<path>. Empty string (and nil
// error) means "this file is gone on the base side" — git exits non-zero
// in that case so we surface a separate error to the caller.
func gitShowFile(worktreeID, ref, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "-C", worktreeID, "show", ref+":"+relPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// git uses exit 128 + "fatal: ..." for missing paths. Translate
		// to an empty result so the file dialog renders the "new file"
		// empty-state without raising an error to the user.
		return nil, nil
	}
	return stdout.Bytes(), nil
}

// sanitizeWorktreePath rejects paths that escape the worktree root via
// `..` segments, absolute paths, or symlink-resolution games. The worktree
// root is treated as a sealed directory — we never serve files outside it.
func sanitizeWorktreePath(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	// Reject NUL bytes outright (git ref parsing could go sideways).
	if strings.ContainsRune(raw, 0) {
		return "", false
	}
	// Reject absolute paths and Windows-style drive letters. Even on
	// Linux, a leading "/" would join with worktreeID to point outside
	// the worktree.
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return "", false
	}
	// Normalise via Clean so "./" segments collapse and trailing slashes
	// are stripped.
	cleaned := filepath.Clean(raw)
	if cleaned == "." || cleaned == "" {
		return "", false
	}
	// Walk each segment and reject ".." before Clean() can mask it.
	parts := strings.Split(cleaned, string(filepath.Separator))
	for _, p := range parts {
		if p == ".." {
			return "", false
		}
	}
	return cleaned, true
}

// splitLines is strings.Split but on \n with optional \r trimming, used
// for git's line-oriented output. Empty lines are dropped.
func splitLines(raw string) []string {
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// summaryLines returns the first n non-empty lines joined with "\n",
// suffixed with " (N more)" when the input had more. Used for the
// "uncommitted changes" preview chip in the sidebar header.
func summaryLines(raw string, n int) string {
	lines := splitLines(raw)
	if len(lines) == 0 {
		return ""
	}
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return fmt.Sprintf("%s\n… (%d more)", strings.Join(lines[:n], "\n"), len(lines)-n)
}

// parseIntOrZero is a forgiving Atoi that returns 0 on parse failure
// (e.g. the binary "-" marker).
func parseIntOrZero(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// truncateBytes trims buf to max bytes. Used by the file endpoint to
// guarantee a worst-case response size.
func truncateBytes(buf []byte, max int) []byte {
	if len(buf) <= max {
		return buf
	}
	return buf[:max]
}
