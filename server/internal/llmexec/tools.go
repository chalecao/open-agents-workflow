package llmexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Tool interface + standard tool set
// ---------------------------------------------------------------------------

// ToolImpl is the contract every server-side tool satisfies. The
// worker holds a slice of these in a registry, looks them up by
// Name() when the model requests a call, and feeds the JSON
// arguments string into Invoke.
//
// ToolImpl implementations are stateless aside from the Worktree
// reference the worker passes in — the registry is shared, the
// per-task state lives in the Worktree. This keeps the dispatch
// path lock-free.
//
// The contract is backend-agnostic: every Invoke call goes
// through the Worktree's forwarding methods, which in turn
// route to whichever Backend (LocalBackend or RemoteBackend)
// was chosen at Init time. The model sees the same tool
// surface whether it's working on a github_repo project, a
// local_directory project, or no project at all.
type ToolImpl interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error)
}

// StandardTools returns the full set of tools the worker wires
// into every tool-enabled task: file ops, a bounded shell, and
// the git ops needed to produce a reviewable commit. The
// order matters for the model's UX — keep related tools
// adjacent so the LLM reads the tool list as a coherent
// workflow (read → write → run → commit).
func StandardTools() []ToolImpl {
	return []ToolImpl{
		&readFileTool{},
		&listDirTool{},
		&writeFileTool{},
		&runShellTool{},
		&gitStatusTool{},
		&gitDiffTool{},
		&gitCommitTool{},
	}
}

// ToolsToWire converts the Go-side ToolImpl values to the wire
// shape the LLM client posts on /chat/completions. Description
// and parameters are passed verbatim — the LLM uses them to
// decide when to call each tool.
func ToolsToWire(tools []ToolImpl) []Tool {
	out := make([]Tool, len(tools))
	for i, t := range tools {
		out[i] = Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		}
	}
	return out
}

// DispatchCall looks up a tool by name and invokes it with the
// supplied arguments. The returned string is what gets sent back
// to the model as a role:"tool" message; errors are also
// stringified (with a "ERROR: " prefix) rather than swallowed so
// the model can self-correct on the next turn.
//
// Name lookups are case-sensitive: the model emits the exact
// name it received in the tools array, so any mismatch is a
// wire bug, not a UX concern.
func DispatchCall(ctx context.Context, wd *Worktree, tools []ToolImpl, name, argsJSON string) (string, error) {
	for _, t := range tools {
		if t.Name() == name {
			out, err := t.Invoke(ctx, wd, json.RawMessage(argsJSON))
			if err != nil {
				return "ERROR: " + err.Error(), nil
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

type readFileTool struct{}

func (readFileTool) Name() string { return "read_file" }
func (readFileTool) Description() string {
	return "Read the contents of a file inside the workdir. " +
		"Paths are relative to the workdir root unless absolute (in which case they " +
		"must still resolve inside the workdir). Returns the raw bytes decoded as UTF-8 " +
		"with line numbers; for files larger than 32 KiB only the first 32 KiB is returned."
}
func (readFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path relative to the workdir root (or absolute, but must resolve inside the workdir)."},
			"max_bytes":{"type":"integer","description":"Optional cap on returned bytes; defaults to 32768."}
		},
		"required":["path"]
	}`)
}
func (readFileTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("read_file: parse args: %w", err)
	}
	if a.Path == "" {
		return "", errors.New("read_file: path is required")
	}
	if a.MaxBytes <= 0 {
		a.MaxBytes = 32 << 10
	}
	rel, err := wd.resolveInWorktree(a.Path)
	if err != nil {
		return "", err
	}
	data, err := wd.readFileOp(ctx, rel, a.MaxBytes)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if len(data) > a.MaxBytes {
		// readFileOp truncated at the backend; surface
		// the suffix the model is used to seeing.
		return data[:a.MaxBytes] + fmt.Sprintf("\n... [truncated at %d bytes; file is larger]", a.MaxBytes), nil
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

type listDirTool struct{}

func (listDirTool) Name() string { return "list_dir" }
func (listDirTool) Description() string {
	return "List the entries of a directory inside the workdir, one per line, " +
		"with a trailing '/' for directories. Hidden entries are included. " +
		"Recursion is bounded to 4 levels; deep trees should be filtered with `find` via run_shell."
}
func (listDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"Directory path relative to the workdir root. Use \".\" for the workdir itself."}
		},
		"required":["path"]
	}`)
}
func (listDirTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("list_dir: parse args: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	rel, err := wd.resolveInWorktree(a.Path)
	if err != nil {
		return "", err
	}
	out, err := wd.listDirOp(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("list_dir: %w", err)
	}
	if out == "" {
		return "(empty directory)", nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

type writeFileTool struct{}

func (writeFileTool) Name() string { return "write_file" }
func (writeFileTool) Description() string {
	return "Create or overwrite a file inside the workdir with the given UTF-8 content. " +
		"Parent directories are created automatically. Recorded in the changed-files audit " +
		"as either 'created' (new path) or 'modified' (overwrote a pre-existing file). " +
		"Refuses to write to paths inside .git/ — the model can commit via git_commit, not by " +
		"hand-editing the git internals."
}
func (writeFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path relative to the workdir root."},
			"content":{"type":"string","description":"UTF-8 file content. The entire file is replaced."}
		},
		"required":["path","content"]
	}`)
}
func (writeFileTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("write_file: parse args: %w", err)
	}
	if a.Path == "" {
		return "", errors.New("write_file: path is required")
	}
	rel, err := wd.resolveInWorktree(a.Path)
	if err != nil {
		return "", err
	}
	if err := wd.writeFileOp(ctx, rel, a.Content); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), rel), nil
}

// ---------------------------------------------------------------------------
// run_shell
// ---------------------------------------------------------------------------

// runShellTool runs a shell command inside the workdir. The cwd
// is the workdir root, so commands see the model-edited tree.
// For LocalBackend the cmd.Dir is set to the workdir; for
// RemoteBackend the daemon sets the cwd to local_path before
// exec — both cases produce the same observable behaviour to
// the model.
//
// Network access is the responsibility of the operator (the
// model could in principle `curl` out; the threat model here
// is "LLM mistypes and nukes the host", not "LLM is actively
// malicious" — same trust level as the daemon's `run_shell`
// tool today).
type runShellTool struct{}

func (runShellTool) Name() string { return "run_shell" }
func (runShellTool) Description() string {
	return "Run a shell command inside the workdir (cwd = workdir root). " +
		"Captures stdout and stderr. Kills the process after the supplied timeout " +
		"(default 30s, max 5m). Use for build / test / git invocations the dedicated " +
		"tools don't cover."
}
func (runShellTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"command":{"type":"string","description":"Shell command line, passed to /bin/sh -c."},
			"timeout_seconds":{"type":"integer","description":"Per-call timeout in seconds. Default 30, max 300."}
		},
		"required":["command"]
	}`)
}
func (runShellTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("run_shell: parse args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("run_shell: command is required")
	}
	timeout := 30 * time.Second
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}
	out, err := wd.runShellOp(ctx, a.Command, timeout)
	if err != nil {
		return "", fmt.Errorf("run_shell: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// git_status / git_diff / git_commit
// ---------------------------------------------------------------------------

type gitStatusTool struct{}

func (gitStatusTool) Name() string { return "git_status" }
func (gitStatusTool) Description() string {
	return "Run `git status --short` in the workdir. Returns the porcelain output " +
		"(\"XY path\" lines for staged, unstaged, and untracked changes)."
}
func (gitStatusTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (gitStatusTool) Invoke(ctx context.Context, wd *Worktree, _ json.RawMessage) (string, error) {
	out, err := wd.gitStatusOp(ctx)
	if err != nil {
		return "", fmt.Errorf("git_status: %w", err)
	}
	if out == "" {
		return "(clean)", nil
	}
	return out, nil
}

type gitDiffTool struct{}

func (gitDiffTool) Name() string { return "git_diff" }
func (gitDiffTool) Description() string {
	return "Run `git diff` in the workdir and return the unified diff. " +
		"Pass staged=true to show staged-but-uncommitted changes; default is unstaged. " +
		"Output is truncated to 64 KiB; ask the model to commit early and inspect via " +
		"git log -p if you need to see more."
}
func (gitDiffTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"staged":{"type":"boolean","description":"If true, show staged changes (git diff --cached). Default false."}
		}
	}`)
}
func (gitDiffTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Staged bool `json:"staged"`
	}
	_ = json.Unmarshal(args, &a)
	out, err := wd.gitDiffOp(ctx, a.Staged)
	if err != nil {
		return "", fmt.Errorf("git_diff: %w", err)
	}
	if out == "" {
		return "(no changes)", nil
	}
	const cap = 64 << 10
	if len(out) > cap {
		return out[:cap] + "\n[truncated at 64 KiB]", nil
	}
	return out, nil
}

type gitCommitTool struct{}

func (gitCommitTool) Name() string { return "git_commit" }
func (gitCommitTool) Description() string {
	return "Stage all changes in the workdir (`git add -A`) and commit with the supplied " +
		"message. Returns the short commit SHA on success. Use this as the model's " +
		"checkpoint — after committing, call git_status to confirm a clean tree, then " +
		"end the conversation with a final summary message. Records the commit in the " +
		"task result so downstream consumers (autopilot runs, issue activities) can " +
		"show it."
}
func (gitCommitTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"message":{"type":"string","description":"Commit message (subject only; the worker appends a tooltrail footer)."}
		},
		"required":["message"]
	}`)
}
func (gitCommitTool) Invoke(ctx context.Context, wd *Worktree, args json.RawMessage) (string, error) {
	var a struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("git_commit: parse args: %w", err)
	}
	if strings.TrimSpace(a.Message) == "" {
		return "", errors.New("git_commit: message is required")
	}
	sha, err := wd.gitCommitOp(ctx, a.Message)
	if err != nil {
		return "", fmt.Errorf("git_commit: %w", err)
	}
	return fmt.Sprintf("committed %s: %s", sha, strings.SplitN(a.Message, "\n", 2)[0]), nil
}

// unused imports kept in case future file walks return to tools.go
var (
	_ = fs.SkipDir
	_ = filepath.Separator
)
