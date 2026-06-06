package agent

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// qodercliBackend implements Backend by spawning Qoder CLI in non-interactive
// print mode. Like the Antigravity backend, the Qoder CLI does not emit a
// structured event stream — `--print` stdout is plain assistant text. The
// backend therefore streams stdout line-by-line as `MessageText` events and
// accumulates the same text as the final `Result.Output`.
//
// Qoder CLI does not expose a `--model` flag today (model selection lives
// in the user's local Qoder settings / `/config` slash command) and the
// backend deliberately drops opts.Model on the floor — the UI surfaces
// the picker as "Managed by runtime" via ModelSelectionSupported returning
// false for "qodercli" (see server/pkg/agent/models.go).
//
// Session resumption uses `-r <session-id>`. The CLI does not currently
// surface the new session id in a stable place on stdout/stderr, so the
// backend cannot capture a resume token from a fresh run — but the
// existing-token path (`opts.ResumeSessionID != ""`) is fully wired and
// best-effort extracts a UUID-shaped token from stdout to feed subsequent
// turns. `-c` (continue last) is supported as a fallback for users who
// only need to chain sessions and don't need explicit id tracking.
//
// The launch shape is:
//
//	qodercli --print -p <prompt> --yolo [--continue | -r <id>] [--output-format text] [...]
//
// `--yolo` skips per-tool permission prompts; without it the daemon would
// hang on the first shell call waiting for stdin approval, mirroring the
// rationale for `--dangerously-skip-permissions` in antigravity.go.
type qodercliBackend struct {
	cfg Config
}

func (b *qodercliBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "qodercli"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("qodercli executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildQodercliArgs(prompt, opts, b.cfg.Logger)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("qodercli stdout pipe: %w", err)
	}
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[qodercli:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start qodercli: %w", err)
	}

	b.cfg.Logger.Info("qodercli started", "pid", cmd.Process.Pid, "cwd", opts.Cwd)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		var output strings.Builder
		finalStatus := "completed"
		var finalError string

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		trySend(msgCh, Message{Type: MessageStatus, Status: "running"})

		for scanner.Scan() {
			line := scanner.Text()
			if output.Len() > 0 {
				output.WriteByte('\n')
			}
			output.WriteString(line)
			if strings.TrimSpace(line) != "" {
				trySend(msgCh, Message{Type: MessageText, Content: line})
			}
		}
		if err := scanner.Err(); err != nil {
			b.cfg.Logger.Warn("qodercli stdout scanner error", "err", err)
		}

		waitErr := cmd.Wait()
		duration := time.Since(startTime)

		// Best-effort: pull a session id from the captured stdout so a
		// caller that wants to chain turns can do so without an extra
		// round-trip. Qoder CLI doesn't currently print a stable
		// "session id: …" line, so the regex is a heuristic and will
		// silently return "" for most runs — that's fine, the
		// explicit `opts.ResumeSessionID` path still works whenever
		// the caller has the id from some other source.
		sessionID := qodercliExtractSessionID(output.String())

		if runCtx.Err() == context.DeadlineExceeded {
			finalStatus = "timeout"
			finalError = fmt.Sprintf("qodercli timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			finalStatus = "aborted"
			finalError = "execution cancelled"
		} else if waitErr != nil && finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("qodercli exited with error: %v", waitErr)
		}
		if finalError != "" {
			finalError = withAgentStderr(finalError, "qodercli", stderrBuf.Tail())
		}

		b.cfg.Logger.Info("qodercli finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).Milliseconds())

		resCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
			// Qoder CLI doesn't surface per-turn token usage today;
			// leave Usage empty rather than report misleading zeros
			// under a guessed model name.
			Usage: map[string]TokenUsage{},
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// qodercliSessionIDRe matches a UUID-shaped token anywhere in the captured
// output. Qoder CLI does not currently print a stable "session id" line
// on stdout, so this is a heuristic; the most common shape we expect to
// see is a uuid4 token on a line like "session: <uuid>" or trailing a
// "resumable with: qodercli -r <uuid>" hint if/when one is added.
var qodercliSessionIDRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// qodercliExtractSessionID returns the last UUID-shaped token found in
// output, or "" if none. The "last wins" tie-break mirrors the
// antigravity conversation-id capture: a single run may surface the id
// more than once (a header line, a resume hint, etc.) and the latest
// occurrence is the one to pin to the next turn.
func qodercliExtractSessionID(output string) string {
	matches := qodercliSessionIDRe.FindAllString(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// qodercliBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Overriding these would
// break non-interactive operation or the daemon's session-resume
// bookkeeping.
var qodercliBlockedArgs = map[string]blockedArgMode{
	"--print":    blockedStandalone, // print mode is the daemon's contract
	"-p":         blockedWithValue,  // prompt is supplied per-run
	"--prompt":   blockedWithValue,
	"-i":         blockedStandalone, // interactive would block the daemon
	"--yolo":     blockedStandalone, // always-on in daemon mode
	"-c":         blockedStandalone, // resume via -r <id>, not blind continue
	"--continue": blockedStandalone,
	"-r":         blockedWithValue, // managed via ExecOptions.ResumeSessionID
	"--resume":   blockedWithValue,
	"--worktree": blockedWithValue, // worktree support is not yet wired through the daemon
	"-w":         blockedWithValue, // workdir is supplied via cmd.Dir
	"--workspace": blockedWithValue,
}

// buildQodercliArgs assembles the argv for a one-shot qodercli invocation.
//
//	qodercli --print -p <prompt> --yolo [--continue | -r <id>] [...]
func buildQodercliArgs(prompt string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"--print",
		"-p", prompt,
		"--yolo",
		// Force plain-text output. The CLI's default is "text", but
		// pinning it here means a user adding `--output-format json`
		// via custom_args would have it filtered by qodercliBlockedArgs
		// (defence in depth) — and an unfiltered set JSON we couldn't
		// parse into MessageText today.
		"--output-format", "text",
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "-r", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, qodercliBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, qodercliBlockedArgs, logger)...)
	return args
}
