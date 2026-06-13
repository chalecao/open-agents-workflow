# Debug Session: llm-agent-never-finish

Status: [RESOLVED — user verified]
Created: 2026-06-07
Symptom: 分配给 issue 的 LLM-based agent 被调用 → 本地 LLM 正常返回 → 但 issue 下 agent 一直不结束 (stuck running)。
User config: `base_url = http://192.168.1.8:1234/v1`

## Root cause (H1 confirmed via static evidence)

**The LLM worker skips the `dispatched → running` transition that the daemon-side flow performs explicitly.**

State machine in `agent_task_queue`:
- `ClaimAgentTask` (sqlc) → `queued → dispatched` ([agent.sql:276-322](file:///Users/caohuanhuan/work/multica/server/pkg/db/queries/agent.sql#L276-L322))
- `StartAgentTask` (sqlc) → `dispatched → running` ([agent.sql:331-334](file:///Users/caohuanhuan/work/multica/server/pkg/db/queries/agent.sql#L331-L334))
- `CompleteAgentTask` (sqlc) → `running → completed` ([agent.sql:351-355](file:///Users/caohuanhuan/work/multica/server/pkg/db/queries/agent.sql#L351-L355), with `WHERE id=$1 AND status='running'`)

Daemon flow (per [daemon.go:1701-1711](file:///Users/caohuanhuan/work/multica/server/internal/handler/daemon.go#L1701-L1711) + [task.go:1035-1053](file:///Users/caohuanhuan/work/multica/server/internal/service/task.go#L1035-L1053)):
1. POST `/tasks/claim` → `dispatched`
2. POST `/tasks/{id}/start` → `running` (calls `TaskService.StartTask` → `StartAgentTask` SQL + analytics + `EventTaskRunning` broadcast)
3. Run upstream work
4. POST `/tasks/{id}/complete` → `completed`

LLM worker flow (per [worker.go:193-203](file:///Users/caohuanhuan/work/multica/server/internal/llmexec/worker.go#L193-L203)):
1. `ClaimTaskForRuntime` → `dispatched`
2. **Step skipped** — no StartTask call, status stays `dispatched`
3. `client.Do` runs (LLM call succeeds)
4. `completer.CompleteTask` → `CompleteAgentTask` SQL with `WHERE status='running'` → **0 rows affected** → `pgx.ErrNoRows`

`CompleteTask` then hits the idempotency branch at [task.go:1124-1135](file:///Users/caohuanhuan/work/multica/server/internal/service/task.go#L1124-L1135):
```go
if errors.Is(err, pgx.ErrNoRows) {
    slog.Info("complete task: already finalized", ...)
    return &existing, nil  // ← returns success, skipping ALL side effects
}
```

So `CompleteTask` returns success and the worker logs nothing. But the side-effect chain NEVER runs:
- ✘ `captureTaskCompleted` (analytics)
- ✘ `createAgentComment` (synthesized agent reply)
- ✘ `ReconcileAgentStatus` (would have set agent → `idle` because the task is now `completed`… except it ISN'T, it's still `dispatched`)
- ✘ `broadcastTaskEvent(protocol.EventTaskCompleted)` (WebSocket)

Result: task stays in `dispatched` forever, agent stays in `working` (because the dispatched task still exists), no completion event reaches the WS hub, UI shows "agent running" forever. Matches user observation exactly.

**Why the existing log `complete task: already finalized` was misleading**: it's written for the parallel-agent race case (two agents finishing the same task). Treating it as success is correct in that case. But for the LLM-worker case, `current_status` will be `dispatched`, not `completed` / `cancelled` / `failed` — a smoking gun we should log distinctly in a follow-up, but for now the SQL fix closes the gap.

## Fix applied

### Files changed
- `server/internal/llmexec/worker.go` —
  - new `TaskStarter` interface (mirrors the existing `TaskClaimer` / `TaskCompleter` pattern)
  - `Worker.starter` field
  - `NewWorker` takes `starter TaskStarter` as 4th param
  - `executeTask` calls `w.starter.StartTask(ctx, task.ID)` first thing; on `pgx.ErrNoRows` (task already cancelled/finalized) log and return; on other errors call `failTask`; on success refresh the local task copy
- `server/cmd/server/main.go` — pass `taskSvc` once more as starter (it's a `*service.TaskService` implementing all three interfaces)

### Behavior after fix
| Event | Code path | Outcome |
|---|---|---|
| Worker claims task | `ClaimTaskForRuntime` → `dispatched` | status='dispatched' |
| Worker starts task | `StartTask` → `running` | status='running', `started_at=now()`, `EventTaskRunning` broadcast, analytics captured |
| LLM call | `client.Do` | LLM responds |
| Worker completes task | `CompleteTask` → `CompleteAgentTask` matches WHERE | status='completed', `completed_at=now()`, `EventTaskCompleted` broadcast, agent comment posted, `ReconcileAgentStatus` → agent='idle' |
| UI | React Query refetch on WS event | "agent running" → "agent completed" within a single tick |

### Test status
- `go build ./...` — clean
- `go vet ./internal/llmexec/...` — clean
- `go test -count=1 ./internal/llmexec/...` — `ok` (0.716s)

## Hypotheses (resolved)

### H1 — `worker.executeTask` 成功路径里漏调 `StartTask`（✅ root cause, fixed）
### H2 — issue 状态联动 (ruled out — the failure point is upstream, in the SQL transaction that never commits)
### H3 — task 被反复 claim / 多次入队 (ruled out — single claim, single execute, single complete)
### H4 — payload envelope 不兼容 (ruled out — `output` field is preserved by `protocol.TaskCompletedPayload.Output`)
### H5 — 启动时遗留的 panic 阶段 running task (ruled out — confirmed via panic stack that previous turn's fix removed all panics, and this issue reproduces on a fresh task)

## User verify steps

1. **Rebuild server**:
   ```bash
   cd server && go build ./... && cd ..
   make server
   ```
2. **Confirm clean boot log** (no panic).
3. **Assign a fresh issue to the LLM-based agent** (or rerun the issue that was stuck).
4. **Watch logs** for the chain:
   ```
   task claimed            task_id=...
   task started            task_id=...   ← new line, was missing before
   llmexec: bump liveness  ...
   task completed          task_id=...   ← now actually fires
   ```
5. **Watch the issue UI**:
   - Before fix: agent indicator stuck at "running" forever
   - After fix: indicator transitions to "completed" within a few seconds of the LLM call returning
6. **DB self-check** (optional):
   ```sql
   SELECT id, status, started_at, completed_at
   FROM agent_task_queue
   WHERE issue_id = '<issue_id>'
   ORDER BY created_at DESC LIMIT 3;
   ```
   Expected: `status='completed'`, `started_at IS NOT NULL`, `completed_at IS NOT NULL` (started_at < completed_at).
