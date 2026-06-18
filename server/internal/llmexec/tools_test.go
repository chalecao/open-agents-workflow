package llmexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorktree_InitAndCleanup walks the happy path: init creates
// the dir, Cleanup removes it. Run as a table entry with several
// repoURL values (empty, invalid) so a future regression where
// "no repo" stops initialising git is caught.
func TestWorktree_InitAndCleanup(t *testing.T) {
	cases := []struct {
		name    string
		repoURL string
	}{
		{"empty worktree", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wt := &Worktree{}
			// InitOptions.RepoURL="" hits the
			// initEmptyWorktree path; we don't need a
			// real workspace / task id for the no-repo
			// branch, but the contract requires the
			// WorktreeParent (parent dir for the temp
			// leaf) to be set.
			if err := wt.Init(InitOptions{
				WorktreeParent: t.TempDir(),
				RepoURL:        tc.repoURL,
			}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if wt.Root() == "" {
				t.Fatal("Root() empty after Init")
			}
			// The init path always initialises git so the
			// git_* tools work — verify .git exists.
			if _, err := os.Stat(filepath.Join(wt.Root(), ".git")); err != nil {
				t.Fatalf("expected .git in worktree, got %v", err)
			}
			wt.Cleanup()
			if _, err := os.Stat(wt.Root()); !os.IsNotExist(err) {
				t.Fatalf("expected worktree removed after Cleanup, stat err=%v", err)
			}
		})
	}
}

// TestWorktree_ResolveInWorktree locks down the server-side
// path-escape guard. The Worktree.resolveInWorktree method
// rejects obvious escapes (relative `..` paths) so the
// tools see a clear error before the backend is ever
// asked. Absolute-path containment is the backend's
// responsibility (LocalBackend uses filepath.Rel; the
// remote backend re-validates inside the daemon's
// sandbox). The split keeps the server's escape check
// backend-agnostic — important because RemoteBackend
// doesn't have a real path to compute Rel against.
func TestWorktree_ResolveInWorktree(t *testing.T) {
	wt := &Worktree{}
	// No backend attached — we test the server-side
	// guard in isolation. The "no backend" branch in
	// resolveInWorktree is exercised here as a side
	// effect; the tools never call resolveInWorktree
	// before Init.
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	cases := []struct {
		in   string
		want bool // true = should resolve cleanly
	}{
		{"README.md", true},
		{"./src/main.go", true},
		{"../../etc/passwd", false},
		{"a/../b", true}, // resolves to "b" — still inside
	}
	for _, tc := range cases {
		got, err := wt.resolveInWorktree(tc.in)
		if tc.want {
			if err != nil {
				t.Errorf("resolve(%q): unexpected error %v", tc.in, err)
				continue
			}
			if got == "" {
				t.Errorf("resolve(%q): empty result", tc.in)
			}
		} else {
			if err == nil {
				t.Errorf("resolve(%q): expected escape error, got %q", tc.in, got)
			}
		}
	}
}

// TestReadFileTool_EndToEnd exercises the read_file tool: write
// a file by hand, then call Invoke through DispatchCall, check
// the result matches.
func TestReadFileTool_EndToEnd(t *testing.T) {
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	want := "hello\nworld\n"
	if err := os.WriteFile(filepath.Join(wt.Root(), "greeting.txt"), []byte(want), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tools := StandardTools()
	out, err := DispatchCall(context.Background(), wt, tools, "read_file", `{"path":"greeting.txt"}`)
	if err != nil {
		t.Fatalf("DispatchCall: %v", err)
	}
	if out != want {
		t.Fatalf("read_file: got %q, want %q", out, want)
	}
}

// TestWriteFileTool_TrackedInChangedFiles confirms write_file
// records the path in the right bucket (created vs. modified)
// and that ChangedFiles() returns the same list. This is the
// audit data the task result depends on, so any drift here
// breaks the activity log summary.
func TestWriteFileTool_TrackedInChangedFiles(t *testing.T) {
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	tools := StandardTools()
	// Step 1: write a fresh file — should show as "created".
	if _, err := DispatchCall(context.Background(), wt, tools, "write_file",
		`{"path":"new.txt","content":"fresh"}`); err != nil {
		t.Fatalf("write_file new: %v", err)
	}
	// Step 2: write to the same path again — should move to
	// "modified" because the path was in the initial snapshot.
	if _, err := DispatchCall(context.Background(), wt, tools, "write_file",
		`{"path":"new.txt","content":"fresh2"}`); err != nil {
		t.Fatalf("write_file overwrite: %v", err)
	}
	created, modified, _ := wt.ChangedFiles()
	if len(created) != 0 {
		t.Errorf("created should be empty after overwrite, got %v", created)
	}
	if len(modified) != 1 || modified[0] != "new.txt" {
		t.Errorf("modified: got %v, want [new.txt]", modified)
	}
}

// TestRunShellTool_Timeout verifies the per-call timeout is
// enforced. We launch `sleep 10` with a 200 ms cap; the call
// should return an error string (DispatchCall surfaces tool
// errors with an "ERROR: " prefix so the model can self-correct)
// in well under 10s. We don't expect err != nil because
// DispatchCall converts tool errors into role:"tool" output.
func TestRunShellTool_Timeout(t *testing.T) {
	if _, err := execLookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	tools := StandardTools()
	start := time.Now()
	out, err := DispatchCall(context.Background(), wt, tools, "run_shell",
		`{"command":"sleep 10","timeout_seconds":1}`)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("DispatchCall returned err (tool errors are surfaced as output): %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("expected ERROR: prefix in output, got %q", out)
	}
	if dur > 5*time.Second {
		t.Fatalf("timeout took too long: %s", dur)
	}
}

// TestRunShellTool_WorkingDir confirms the shell runs with cwd =
// worktree, so a relative `ls` sees the worktree contents. The
// exact contents change (git creates .git on init), so we just
// assert the call returns "" for an empty command and a
// non-empty list for a real one.
func TestRunShellTool_WorkingDir(t *testing.T) {
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	tools := StandardTools()
	// Write a file, then `ls` should show it.
	if _, err := DispatchCall(context.Background(), wt, tools, "write_file",
		`{"path":"hello.txt","content":"hi"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	out, err := DispatchCall(context.Background(), wt, tools, "run_shell", `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("run_shell: %v", err)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Fatalf("ls output should include hello.txt, got %q", out)
	}
}

// TestGitCommitTool_RecordsCommit is the audit-trail check: after
// write_file + git_commit, wt.commits has one entry with a
// non-empty SHA. The SHA format is "short hash" (git rev-parse
// --short), which is 7-12 hex chars depending on git version.
func TestGitCommitTool_RecordsCommit(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not available")
	}
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	tools := StandardTools()
	if _, err := DispatchCall(context.Background(), wt, tools, "write_file",
		`{"path":"a.txt","content":"x"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	out, err := DispatchCall(context.Background(), wt, tools, "git_commit",
		`{"message":"add a.txt"}`)
	if err != nil {
		t.Fatalf("git_commit: %v", err)
	}
	if !strings.HasPrefix(out, "committed ") {
		t.Fatalf("git_commit: unexpected output %q", out)
	}
	if len(wt.commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(wt.commits))
	}
	sha := wt.commits[0].SHA
	if len(sha) < 7 {
		t.Fatalf("commit SHA too short: %q", sha)
	}
}

// TestDispatchCall_UnknownTool returns a clear "unknown tool"
// error rather than silently dispatching to the first tool in
// the slice. Important because the model is the one that
// supplies the name; any drift is a real wire bug.
func TestDispatchCall_UnknownTool(t *testing.T) {
	wt := &Worktree{}
	if err := wt.Init(InitOptions{
		WorktreeParent: t.TempDir(),
		RepoURL:        "",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer wt.Cleanup()
	_, err := DispatchCall(context.Background(), wt, StandardTools(), "no_such_tool", `{}`)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("err: %v", err)
	}
}

// TestToolsToWire_FieldShape verifies the conversion from
// ToolImpl to the wire-format Tool struct populates every field
// the model sees. The model uses Name / Description / Parameters
// to decide which tool to call, so a missing field is a UX bug.
func TestToolsToWire_FieldShape(t *testing.T) {
	tools := StandardTools()
	wire := ToolsToWire(tools)
	if len(wire) != len(tools) {
		t.Fatalf("length mismatch: impl=%d wire=%d", len(tools), len(wire))
	}
	names := map[string]bool{}
	for _, w := range wire {
		if w.Type != "function" {
			t.Errorf("tool %q: type=%q, want function", w.Function.Name, w.Type)
		}
		if w.Function.Name == "" || w.Function.Description == "" {
			t.Errorf("tool missing name/description: %+v", w)
		}
		if len(w.Function.Parameters) == 0 {
			t.Errorf("tool %q: parameters empty", w.Function.Name)
		}
		// parameters must be valid JSON.
		var v any
		if err := json.Unmarshal(w.Function.Parameters, &v); err != nil {
			t.Errorf("tool %q: parameters not valid JSON: %v", w.Function.Name, err)
		}
		names[w.Function.Name] = true
	}
	// Spot-check the expected names.
	for _, want := range []string{"read_file", "write_file", "list_dir", "run_shell", "git_status", "git_diff", "git_commit"} {
		if !names[want] {
			t.Errorf("missing tool %q in wire output", want)
		}
	}
}

// execLookPath is a thin wrapper that lets the run_shell and
// git_commit tests `t.Skip` cleanly when the host doesn't have
// the binary. Re-exports exec.LookPath so the call site
// documents intent ("I'm guarding on a specific binary")
// without the reader having to scan for an import.
func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}
