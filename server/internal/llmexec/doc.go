// Package llmexec implements the server-side LLM execution worker.
//
// The worker polls `agent_task_queue` for tasks bound to `openai-http`
// runtimes (the synthetic runtime rows the LLM provider handlers
// auto-create at workspace setup time) and executes them by making
// OpenAI-compatible HTTP calls to the user-configured endpoint. The
// output of the call is fed back to the task system via the same
// TaskService.CompleteTask / FailTask methods the daemon uses, so
// downstream observers (realtime, activity log, dashboard) cannot
// tell the difference between a daemon-driven run and an LLM-driven
// one.
//
// The package is intentionally small and dependency-light so it can
// be wired in lazily: a nil *Box secretbox turns the worker into a
// no-op (Run returns nil, ExecuteOnce returns ErrDisabled). The router
// only constructs a Worker when MULTICA_LLM_SECRET_KEY is configured,
// so deployments that have not opted in to LLM execution are
// unaffected.
package llmexec

import "errors"

// ErrDisabled is returned by Worker.ExecuteOnce and Worker.Run when
// the LLM execution subsystem is not configured (no
// MULTICA_LLM_SECRET_KEY set at boot, or secretbox.Box was nil).
// Callers MUST treat this as a non-fatal "feature off" signal: it
// is not a transient error and is not retried.
var ErrDisabled = errors.New("llmexec: not configured (set MULTICA_LLM_SECRET_KEY)")
