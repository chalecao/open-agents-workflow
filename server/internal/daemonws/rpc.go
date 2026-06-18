package daemonws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// RPC request/response machinery on top of the existing task-wakeup
// WebSocket. The server is the only writer; the daemon responds. Many
// requests can be in flight at once, multiplexed by RequestID in the
// payload.
//
// Concurrency model:
//   - The Hub owns a single `pending` map keyed by RequestID, guarded by
//     `pendingMu`. The map is touched only on the send path (register
//     before write) and the receive path (unregister after delivery),
//     both of which are short critical sections.
//   - Send path: Request() picks a client for runtimeID, registers a
//     buffered channel under the request_id, and enqueues the frame
//     onto the client's send channel. If the send channel is full the
//     client is dead and we abort the request with a transport error
//     before the daemon ever sees it.
//   - Receive path: handleFrame() unmarshals the response and dispatches
//     to the registered channel. If the channel isn't registered (e.g.
//     ctx already cancelled) the response is logged and dropped — never
//     blocking the read pump.
//   - Cancellation: when the caller's ctx is cancelled, Request()
//     removes the pending entry. Any later response for that id is
//     dropped on arrival. This keeps a stuck daemon from holding the
//     channel open forever.

// ErrDaemonNotReachable is returned by Request when no WebSocket client
// is currently registered for the requested runtime. The caller is
// expected to surface this to the LLM tool loop as a tool-level
// error so the model knows the tool session is gone.
var ErrDaemonNotReachable = errors.New("daemonws: no connected client for runtime")

// ErrDaemonRPCFailed is the umbrella for an OK=false reply from the
// daemon. Code carries the daemon's classification (e.g.
// "unknown_method", "session_not_found", "shell_timeout"); Message
// carries the human-readable reason.
type ErrDaemonRPCFailed struct {
	Code    string
	Message string
}

func (e *ErrDaemonRPCFailed) Error() string {
	if e.Code == "" {
		return "daemonws: " + e.Message
	}
	return "daemonws: [" + e.Code + "] " + e.Message
}

// ErrDaemonRPCTimeout is returned when ctx is done before the daemon
// responds. The caller should treat this the same as a tool timeout
// and surface "ERROR: rpc timeout" to the model.
var ErrDaemonRPCTimeout = errors.New("daemonws: rpc timeout")

// Request sends an RPC frame to the daemon currently connected for
// runtimeID and blocks until the daemon replies, the context is
// cancelled, or the call times out. The request_id is chosen by the
// caller so the LLM worker can cancel in-flight tool calls when the
// user aborts the task.
//
// ErrDaemonNotReachable is returned if no client is currently
// connected for the runtime. ErrDaemonRPCTimeout is returned if the
// ctx is cancelled before the reply arrives. ErrDaemonRPCFailed
// (with Code set) is returned for OK=false replies; the underlying
// daemon code decides the classification.
func (h *Hub) Request(
	ctx context.Context,
	runtimeID string,
	requestID string,
	method string,
	workspaceID string,
	taskID string,
	sessionID string,
	args json.RawMessage,
) (json.RawMessage, error) {
	if h == nil {
		return nil, ErrDaemonNotReachable
	}
	if requestID == "" {
		return nil, errors.New("daemonws: request_id required")
	}
	if method == "" {
		return nil, errors.New("daemonws: method required")
	}
	if args == nil {
		// Always serialise as {} so the daemon never has to
		// branch on "absent vs empty object". Cheap and
		// keeps the JSON shape predictable.
		args = json.RawMessage("{}")
	}

	// Pick a live client for the runtime. byRuntime is read-locked
	// for this entire call; register below grabs the write lock for
	// the pending map. The two locks are independent so a slow
	// daemon can't hold up a hub-unrelated lookup.
	h.mu.RLock()
	clients := h.byRuntime[runtimeID]
	var c *client
	for candidate := range clients {
		c = candidate
		break
	}
	h.mu.RUnlock()
	if c == nil {
		return nil, ErrDaemonNotReachable
	}

	reply := make(chan protocol.DaemonRPCResponsePayload, 1)
	h.pendingMu.Lock()
	// Defensive: if the caller reused a request_id by mistake, the
	// later Request would race the earlier one. Drop the prior
	// waiter; it will get ErrDaemonRPCFailed when its context
	// eventually fires.
	if prev, ok := h.pending[requestID]; ok {
		close(prev)
		delete(h.pending, requestID)
	}
	h.pending[requestID] = reply
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		if cur, ok := h.pending[requestID]; ok && cur == reply {
			delete(h.pending, requestID)
		}
		h.pendingMu.Unlock()
	}()

	frame, err := json.Marshal(protocol.Message{
		Type: protocol.EventDaemonRPCRequest,
		Payload: mustMarshalRaw(protocol.DaemonRPCRequestPayload{
			RequestID:   requestID,
			Method:      method,
			WorkspaceID: workspaceID,
			TaskID:      taskID,
			SessionID:   sessionID,
			Args:        args,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("daemonws: marshal request: %w", err)
	}

	// Send on the client's outbound channel. If the buffer is full
	// the daemon is backpressured or stuck — treat it as not
	// reachable so the LLM worker can fail fast and the user sees
	// a useful error rather than a silent hang.
	select {
	case c.send <- frame:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// Try to evict a slow client so subsequent requests on
		// other runtimes don't all pay the same penalty; the
		// read pump will reopen the connection on its own.
		go func(cl *client) {
			cl.conn.Close()
		}(c)
		return nil, ErrDaemonNotReachable
	}

	select {
	case resp, ok := <-reply:
		if !ok {
			// The pending slot was stolen by a later
			// Request reusing the same request_id; rare
			// bug in the caller, surface it.
			return nil, errors.New("daemonws: pending slot preempted")
		}
		if !resp.OK {
			return nil, &ErrDaemonRPCFailed{Code: resp.Code, Message: resp.Error}
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ErrDaemonRPCTimeout
	}
}

// handleRPCResponse is the receive-side half of the Request contract.
// Called by the client's readPump after it has decoded a
// daemon:rpc_response frame. Looks up the registered channel and
// delivers the payload; the registered channel is unblocked and the
// Request caller's goroutine returns.
//
// A response for an unknown request_id (timed out, or caller's ctx
// already cancelled) is logged at debug and dropped — it must never
// block the read pump.
func (h *Hub) handleRPCResponse(payload json.RawMessage) {
	var resp protocol.DaemonRPCResponsePayload
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Debug("daemonws: invalid rpc_response payload", "error", err)
		return
	}
	if resp.RequestID == "" {
		slog.Debug("daemonws: rpc_response missing request_id")
		return
	}
	h.pendingMu.Lock()
	ch, ok := h.pending[resp.RequestID]
	if !ok {
		h.pendingMu.Unlock()
		slog.Debug("daemonws: rpc_response for unknown request_id",
			"request_id", resp.RequestID)
		return
	}
	// We hold the pending entry open until the receiver has
	// accepted the payload so a duplicate response (daemon
	// retransmits, or a buggy handler replies twice) doesn't go to
	// the next Request reusing the same id. We optimistically copy
	// the channel and remove; if the send blocks because the
	// receiver already gave up, the channel is full and the
	// duplicate is dropped.
	select {
	case ch <- resp:
		delete(h.pending, resp.RequestID)
	default:
		slog.Debug("daemonws: rpc_response receiver not waiting",
			"request_id", resp.RequestID)
	}
	h.pendingMu.Unlock()
}

// pendingRPCStats returns the number of in-flight RPCs — used by the
// hub's metrics hook if a test wants to assert that the pending map
// drains. Not a hot-path method.
func (h *Hub) pendingRPCStats() int {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	return len(h.pending)
}
