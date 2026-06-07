# Debug Session: llm-provider-offline

Status: [ROOT CAUSE — second bug found via panic stack, fix applied]
Created: 2026-06-07
Symptom: 运行时（runtime）显示"离线"（offline），但本地 LM Studio / 大模型服务实际可达。
User config: `base_url = http://192.168.1.8:1234/api` (later corrected to `/v1`)

## Real root cause — `worker.go:uuidString` panic

After the user restarted with `MULTICA_LLM_SECRET_KEY` set, the worker started
(`20:02:28.740 INF llmexec worker starting`) but then panicked on the first
task execution:

```
20:04:09.304 INF llmexec worker stopped
panic: runtime error: index out of range [36] with length 36
goroutine 60 [running]:
github.com/multica-ai/multica/server/internal/llmexec.uuidString(...)
	/Users/caohuanhuan/work/multica/server/internal/llmexec/worker.go:412
github.com/multica-ai/multica/server/internal/llmexec.(*Worker).executeTask(...)
	/Users/caohuanhuan/work/multica/server/internal/llmexec/worker.go:262
...
```

The hand-rolled `uuidString` at [worker.go:400-414](file:///Users/caohuanhuan/work/multica/server/internal/llmexec/worker.go#L400-L414) (pre-fix) had an off-by-one in its index formula:

```go
out := make([]byte, 36)
for i, b := range u.Bytes {           // i = 0..15
    switch i {
    case 4, 6, 8, 10:
        out[i*2+i/2-1] = '-'
    }
    out[i*2+i/2]   = hex[b>>4]        // i=15: 30+7=37, slice len 36 → panic
    out[i*2+i/2+1] = hex[b&0x0f]      // i=15: 38 → panic
}
```

At `i=15`, `i*2+i/2+1 = 38` is past the end of the 36-byte buffer. Every call
to `uuidString` with a fully-populated 16-byte UUID panics. The function was
duplicated locally instead of using the existing correct
[util.UUIDToString](file:///Users/caohuanhuan/work/multica/server/internal/util/pgx.go#L41-L57).

**Why this caused the offline display**: 10 call sites in worker.go
(lines 195, 261, 271, 277, 283, 305, 345, 357, 375, 380, 392 in pre-fix file)
all touched `uuidString`. The keep-alive error paths, the bump-liveness
error path, and the result-marshal path all panic the worker. Once the worker
dies, no one refreshes `last_seen_at`, and the 150s sweeper flips the
runtime to offline. Since the user only saw the symptom (offline) and not the
crash (it scrolled off, only the most recent task panic was visible), they
re-fetched the page and saw the stale `status='offline'` row.

**Secondary root cause (H0)**: `MULTICA_LLM_SECRET_KEY` was unset, so the
worker was disabled at boot. Setting it (added to .env earlier) unblocks the
worker startup, which then trips on the panic above. Both are required to
fix the offline display.

## Fix applied

### Files changed
- `server/pkg/db/queries/llm_worker.sql` — new `ListRuntimesByProvider` (no status filter)
- `server/pkg/db/generated/llm_worker.sql.go` — generated binding for the new query
- `server/internal/llmexec/openai_client.go` — new `Ping(ctx, baseURL, apiKey)` method (GET `{baseURL}/models`, nil on 2xx)
- `server/internal/llmexec/worker.go` —
  - removed hand-rolled `uuidString` (buggy); replaced 10 call sites with `util.UUIDToString`
  - added util import
  - new `KeepAliveInterval` (60s) / `KeepAliveTimeout` (10s) fields on Worker
  - new `bumpLiveness` / `keepAliveOnce` methods
  - `Run` drives parallel keep-alive ticker
  - `executeTask` calls `bumpLiveness` on successful upstream call
- `server/internal/llmexec/prompts.go` — updated stale comment that referenced the local `uuidString`
- `server/internal/llmexec/openai_client_test.go` — new `TestOpenAIClient_Ping_Success` / `_StripsTrailingSlash` / `_Non2xx`
- `.env.example` and `.env` — `MULTICA_LLM_SECRET_KEY` documentation + value (already done in earlier turn)

### Code path summary after the fix
| Event | Code path | Outcome |
|---|---|---|
| Worker boot | `Run` sees `box != nil`; goroutine starts | INF `llmexec worker starting` |
| Keep-alive tick (every 60s) | `keepAliveOnce` → `ListRuntimesByProvider` → `Ping` → `MarkAgentRuntimeOnline` | `status='online'`, `last_seen_at=now()` |
| Successful task | `executeTask` → `client.Do` (success) → `bumpLiveness` → `MarkAgentRuntimeOnline` | `last_seen_at=now()` (free signal) |
| URL breaks | `Ping` returns non-2xx → `SetAgentRuntimeOffline` (only if currently online) | `status='offline'` |
| URL fixed | next `Ping` succeeds → `MarkAgentRuntimeOnline` | auto-recovers to online |
| Sweeper (cmd/server/runtime_sweeper.go) | 150s stale window | never trips because keep-alive refreshes within 60s |

### Test status
- `go build ./...` — clean
- `go vet ./internal/llmexec/...` — clean
- `go test -count=1 ./internal/llmexec/...` — `ok` (0.402s)
- `util.UUIDToString` — pre-existing test at [pgx_test.go:49](file:///Users/caohuanhuan/work/multica/server/internal/util/pgx_test.go#L49) covers the round-trip

## Hypotheses (resolved)

### H0 — `MULTICA_LLM_SECRET_KEY` unset (✅ fixed — env var added in earlier turn)
### H1 — `openai-http` 运行时被 stale sweeper 误判为 offline（✅ fixed — keep-alive refreshes last_seen_at）
### H2 — `base_url` 路径错配（✅ confirmed — URL changed to `/v1`, curl returns 200）
### H3 — LAN/防火墙层不可达（✅ confirmed reachable from server）
### H4 — UI 渲染的"离线"不是 `status='offline'`（✅ ruled out — runtime.status field is direct passthrough）
### H5 — Worker 进程 panic（✅ root cause, fixed — buggy `uuidString` removed, all 10 call sites use `util.UUIDToString`）

## User verify steps (full sequence)

1. **Rebuild server**:
   ```bash
   cd server && go build ./... && cd ..
   make server   # or your usual boot command
   ```
2. **Confirm boot log** is clean (no panic, no stack trace).
3. **Confirm `MULTICA_LLM_SECRET_KEY` is set** in `.env` (added in earlier turn).
4. **Confirm base_url is `/v1`** in the LLM Providers UI.
5. **Wait 60s** for the first keep-alive tick. Runtime should be `status='online'`.
6. **Refresh the runtime page** in the UI (Ctrl/Cmd+Shift+R). Should show online.
7. **Optional self-check**:
   ```bash
   curl http://192.168.1.8:1234/v1/models -s -o /dev/null -w "%{http_code}\n"
   # Should print 200
   ```
8. **Stop LM Studio**, wait 60–90s, runtime should flip to offline in the UI.
9. **Restart LM Studio**, wait ≤60s, runtime should auto-recover to online.
