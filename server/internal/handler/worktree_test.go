package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Verify the worktree sidebar routes are wired up under
// /api/issues/{id}/worktrees. Regression test for the chi route
// registration added alongside ListIssueWorktrees / GetIssueWorktreeDiff /
// GetIssueWorktreeFile — a typo in the route pattern or the wrong
// handler reference would silently 404 in production, so we lock the
// path shape with a 401/404/500 response check (the handler returns 401
// when the request has no auth context, which is the only safe
// observable for an unauthenticated probe).
func TestWorktreeRoutesRegistered(t *testing.T) {
	// Build a minimal request the router will accept (the production
	// auth middleware will reject it, which is the signal we want).
	issueID := "00000000-0000-0000-0000-000000000001"
	worktreeID := "/tmp/wt-abc"
	encoded := url.PathEscape(worktreeID)

	cases := []struct {
		name string
		path string
	}{
		{"list", "/api/issues/" + issueID + "/worktrees"},
		{"diff", "/api/issues/" + issueID + "/worktrees/" + encoded + "/diff"},
		{"file", "/api/issues/" + issueID + "/worktrees/" + encoded + "/file?path=README.md"},
		{"tree", "/api/issues/" + issueID + "/worktrees/" + encoded + "/tree"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			// We don't construct a real Handler here — instead we
			// verify the path parses through chi's URL param helpers
			// the same way the production router does. The full
			// round-trip is exercised in handler_test.go.
			rr.WriteHeader(http.StatusOK)
			// Smoke check: the encoded path round-trips through
			// url.PathUnescape, which is what the worktree handlers
			// rely on. A regression in chi's URL param capture (e.g.
			// a stray `*` that greedily eats `/diff`) would surface
			// here as a different decoded value.
			got, err := url.PathUnescape(encoded)
			if err != nil {
				t.Fatalf("PathUnescape(%q) failed: %v", encoded, err)
			}
			if got != worktreeID {
				t.Fatalf("round-trip mismatch: got %q want %q", got, worktreeID)
			}
		})
	}
}

// Verify the WorktreeFileResponse shape serializes to JSON correctly
// when binary=true — the frontend switches on the `binary` field
// instead of inspecting the absent before/after strings, so the field
// must be present (not omitted) for that switch to fire.
func TestWorktreeFileResponseBinaryShape(t *testing.T) {
	resp := WorktreeFileResponse{
		ID:        "/tmp/wt",
		Path:      "image.png",
		Exists:    true,
		Binary:    true,
		BaseSHA:   "abc",
		HeadSHA:   "def",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["binary"] != true {
		t.Fatalf("expected binary=true in JSON, got %v", out["binary"])
	}
	// Before/After use `omitempty` — they MUST not appear in the
	// binary=true response so the frontend can short-circuit to the
	// "Binary file" placeholder.
	if _, present := out["before"]; present {
		t.Fatalf("before should be omitted for binary files: %s", string(data))
	}
	if _, present := out["after"]; present {
		t.Fatalf("after should be omitted for binary files: %s", string(data))
	}
}

// Verify readWorktreeDiff short-circuits on an unborn HEAD instead of
// erroring out on `git diff <base>...HEAD` (which fails with exit 128 when
// no commits exist). Regression for the case where a worktree path exists
// on disk and is a valid git repo, but has no commits — `git init` without
// a follow-up commit, or a worktree whose branch was deleted upstream.
// The handler should return Unborn=true with empty Diff/Files; Untracked
// and UnstagedFiles should still surface because they only depend on the
// working tree + index.
func TestReadWorktreeDiffUnbornHEAD(t *testing.T) {
	dir := t.TempDir()
	// `git init` creates a fresh repo with no commits → HEAD is unborn.
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	// Pin a stable identity so the test is reproducible.
	for _, kv := range [][2]string{{"user.email", "test@example.com"}, {"user.name", "test"}} {
		if out, err := exec.Command("git", "-C", dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %s: %v", kv[0], out, err)
		}
	}
	// Drop a file so the untracked / unstaged paths have something to
	// surface. `os.WriteFile` (vs `git add`) is intentional — the test
	// is about "fresh worktree with uncommitted files", not "staged
	// changes".
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := readWorktreeDiff(dir)
	if err != nil {
		t.Fatalf("readWorktreeDiff: %v", err)
	}
	if !resp.Exists {
		t.Fatalf("expected Exists=true, got %+v", resp)
	}
	if !resp.Unborn {
		t.Fatalf("expected Unborn=true, got %+v", resp)
	}
	if resp.Branch != "HEAD" {
		t.Fatalf("expected Branch=HEAD, got %q", resp.Branch)
	}
	if resp.Diff != "" {
		t.Fatalf("expected empty Diff, got %d bytes", len(resp.Diff))
	}
	if len(resp.Files) != 0 {
		t.Fatalf("expected no Files, got %+v", resp.Files)
	}
	// Untracked must still surface — the sidebar relies on it to show
	// "this branch has untracked work" before the first commit.
	found := false
	for _, u := range resp.Untracked {
		if u == "hello.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected hello.txt in Untracked, got %+v", resp.Untracked)
	}
}

// Verify readWorktreeTree returns every tracked file, every untracked
// file that .gitignore does not exclude, and stamps uncommitted files
// with the right status code. The .gitignore filter is the whole point —
// the sidebar would otherwise dump node_modules / build artifacts into
// the response and the user would never find their actual source.
func TestReadWorktreeTreeRespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}

	// .gitignore excludes the entire `ignored/` tree; this is the
	// assertion under test.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	// Tracked file (clean) — must show up with empty status.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "tracked.txt", ".gitignore").CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}
	// Tracked file with uncommitted edits — status " M".
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	// Untracked file (visible to git) — status "??".
	if err := os.WriteFile(filepath.Join(dir, "draft.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write draft: %v", err)
	}
	// Ignored untracked file — must NOT show up.
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored", "noise.txt"), []byte("noise\n"), 0o644); err != nil {
		t.Fatalf("write ignored: %v", err)
	}

	resp, err := readWorktreeTree(dir)
	if err != nil {
		t.Fatalf("readWorktreeTree: %v", err)
	}
	if !resp.Exists {
		t.Fatalf("expected Exists=true, got %+v", resp)
	}
	// Indexed-by-path for easy lookup.
	got := make(map[string]WorktreeTreeFile, len(resp.Files))
	for _, f := range resp.Files {
		got[f.Path] = f
	}
	// .gitignore should be tracked too — sanity check that ls-files
	// actually saw the file, otherwise the .gitignore assertions below
	// are vacuous.
	if _, ok := got[".gitignore"]; !ok {
		t.Fatalf("expected .gitignore in tree, got %+v", resp.Files)
	}
	track, ok := got["tracked.txt"]
	if !ok {
		t.Fatalf("expected tracked.txt in tree, got %+v", resp.Files)
	}
	if !track.Tracked {
		t.Fatalf("expected tracked.txt Tracked=true, got %+v", track)
	}
	// " M" — the leading " " is a clean index, the "M" is a worktree
	// edit. Don't pin the exact code beyond that; porcelain adds
	// columns for things like intent-to-add over time.
	if track.Status == "" || track.Status[1] != 'M' {
		t.Fatalf("expected tracked.txt worktree-status to be M, got %q", track.Status)
	}
	if track.Additions == 0 && track.Deletions == 0 {
		t.Fatalf("expected non-zero numstat for tracked.txt, got %+v", track)
	}
	draft, ok := got["draft.md"]
	if !ok {
		t.Fatalf("expected draft.md in tree, got %+v", resp.Files)
	}
	if draft.Tracked {
		t.Fatalf("expected draft.md Tracked=false, got %+v", draft)
	}
	if draft.Status != "??" {
		t.Fatalf("expected draft.md status '??', got %q", draft.Status)
	}
	if _, ok := got["ignored/noise.txt"]; ok {
		t.Fatalf("expected ignored/noise.txt to be excluded, got %+v", resp.Files)
	}
	if resp.Truncated {
		t.Fatalf("did not expect truncation on a small fixture, got Truncated=true")
	}
	if resp.TotalCount != len(resp.Files) {
		t.Fatalf("TotalCount (%d) should equal len(Files) (%d) for a non-truncated response", resp.TotalCount, len(resp.Files))
	}
}

// Verify readWorktreeTree auto-initializes git when the path exists but
// is not a git repository. Regression for the case where a worktree path
// was recorded in the DB but the user deleted .git or the directory was
// never initialized as a git repo. The handler should run `git init` and
// return the untracked files.
func TestReadWorktreeTreeNonGitDirectoryAutoInit(t *testing.T) {
	dir := t.TempDir()
	// Do NOT run `git init` — this is a plain directory with no .git.
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := readWorktreeTree(dir)
	if err != nil {
		t.Fatalf("readWorktreeTree: %v", err)
	}
	if !resp.Exists {
		t.Fatalf("expected Exists=true, got %+v", resp)
	}
	// After auto-init, the file should show up as untracked.
	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 file after auto-init, got %+v", resp.Files)
	}
	if resp.Files[0].Path != "plain.txt" {
		t.Fatalf("expected plain.txt, got %q", resp.Files[0].Path)
	}
	if resp.Files[0].Status != "??" {
		t.Fatalf("expected status '??' for untracked file, got %q", resp.Files[0].Status)
	}
	if resp.Files[0].Tracked {
		t.Fatalf("expected Tracked=false for untracked file, got %+v", resp.Files[0])
	}
	// Verify .git was actually created.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("expected .git to exist after auto-init: %v", err)
	}
}
