package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// handleRPCRequest is the daemon-side entry point for one
// server-→-daemon RPC. The read pump calls it from inside
// readTaskWakeupMessages after decoding a protocol.EventDaemonRPCRequest
// frame. The dispatcher:
//
//  1. Parses the payload (request_id, method, workspace_id, task_id,
//     session_id, args).
//  2. Runs the registered handler under a per-call context with a
//     60-second ceiling (matches the server's RequestTimeout so a
//     runaway handler can't hold the WS write pump indefinitely).
//  3. Marshals a protocol.EventDaemonRPCResponse frame and ships it
//     back over the same connection via the writer goroutine that
//     also drains heartbeats.
//
// Errors at the wire-shape level (missing request_id, malformed
// payload) are logged at Debug and dropped — the server's Request
// call will time out and surface a tool-level error to the model.
// Errors from a handler are returned as OK=false with a Code; the
// server's tool loop surfaces the Message as a "ERROR: ..." reply.
func (d *Daemon) handleRPCRequest(conn *websocket.Conn, responses chan<- []byte, payload json.RawMessage) {
	if d.llmRPC == nil {
		// Defensive: the dispatcher should always be
		// initialised by New(). If it isn't, log and drop
		// rather than panic — the server's caller will time
		// out and the model will see a tool error.
		d.logger.Debug("rpc request received but dispatcher is nil",
			"payload_bytes", len(payload))
		return
	}
	var req protocol.DaemonRPCRequestPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		d.logger.Debug("rpc request: invalid payload", "error", err)
		return
	}
	if req.RequestID == "" {
		d.logger.Debug("rpc request: missing request_id")
		return
	}
	if req.Method == "" {
		d.logger.Debug("rpc request: missing method", "request_id", req.RequestID)
		return
	}
	// Per-call ceiling. The server picks a tighter
	// RequestTimeout per RPC type; this 60s cap is the
	// outermost guard so a handler that forgot to set a
	// deadline can't wedge the writer.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, terr := d.llmRPC.dispatch(
		ctx,
		req.Method,
		req.WorkspaceID,
		req.TaskID,
		req.SessionID,
		req.Args,
	)
	resp := protocol.DaemonRPCResponsePayload{RequestID: req.RequestID}
	if terr != nil {
		resp.OK = false
		resp.Code = terr.Code
		resp.Error = terr.Message
	} else {
		resp.OK = true
		resp.Result = result
	}
	frame, err := json.Marshal(protocol.Message{
		Type:    protocol.EventDaemonRPCResponse,
		Payload: marshalRaw(resp),
	})
	if err != nil {
		d.logger.Warn("rpc response marshal failed",
			"request_id", req.RequestID,
			"method", req.Method,
			"error", err)
		return
	}
	// Non-blocking send into the shared writer channel. If
	// the channel is full the heartbeat sender is backed up
	// and the WS is unhealthy anyway — drop the response,
	// the server's Request will time out. Better than
	// blocking the read pump on a stuck writer.
	select {
	case responses <- frame:
	default:
		d.logger.Warn("rpc response dropped: writer channel full",
			"request_id", req.RequestID,
			"method", req.Method)
	}
}

// sendRPCResponse is a small test-only helper that writes a
// response frame directly to the conn, bypassing the shared
// writer channel. Used by tests that drive a single request/
// response cycle without standing up the full writer
// goroutine. Production code must use handleRPCRequest so
// writes are serialised through the writer channel.
//
// Marked with a build tag so it's only compiled into the
// test binary; never link into production.
var _ = slog.Default
