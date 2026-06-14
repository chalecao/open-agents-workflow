<div align="center">

# MultiAgent

**让多个 AI agent 像一支真正的团队一样协作解决问题。**

开源的多 agent 协作平台。配置并行 / 串行工作流、保证任务高效执行、把每一次解决方案沉淀为可复用技能。

**[English](README.md) | 简体中文**

</div>

## MultiAgent 解决什么问题?

一个 agent 处理一个任务时,你只能串行等待。真实工作中,问题需要被拆解、分发、并行处理、回收结果——这正是 **MultiAgent** 的设计目标:把多个 AI agent 组成一支可配置的协作团队。

- **并行协作** —— Squad 模式下,leader agent 根据问题内容 `@` 多个 specialist 并行处理
- **串行协作** —— 一个 agent 处理完后由 leader 重新触发,基于上一轮结果决定下一步交给谁
- **稳定路由** —— 直接 `@FrontendTeam` 而不是 `@小张或小李`,团队成员变更不影响任务分配
- **高效运行** —— 任务队列、状态机、自动重试、超时控制、会话恢复一应俱全

支持 **Claude Code**、**Codex**、**GitHub Copilot CLI**、**OpenClaw**、**OpenCode**、**Hermes**、**Gemini**、**Pi**、**Cursor Agent**、**Kimi**、**Kiro CLI**、**Antigravity** 等 12 种 AI 编码工具。

---

## 核心能力

| 能力 | 做什么 |
|------|--------|
| **Agent 即队友** | 像分配给同事一样把 issue 分给 agent,自动执行、汇报阻塞、提交代码 |
| **Squad(小队)** | leader agent + 多个 specialist,按问题内容自动派发,实现稳定的并行协作 |
| **串行工作流** | 通过 issue 评论串联 agent——A 完成后,leader 读结果再决定派给 B 或 C |
| **任务状态机** | `queued → dispatched → running → completed/failed`,30 秒扫描一次,异常自动重试 |
| **自动重试** | `timeout` / `runtime_offline` / `runtime_recovery` 自动重跑,最多 2 次 |
| **会话恢复** | 自动重试时复用 session,避开基础设施故障导致的中断(Antigravity、Claude Code、Copilot 等 8 个 provider 已支持) |
| **超时控制** | 派发 5 分钟、运行 2.5 小时,超时即失败并按规则重试 |
| **Autopilot(自动化)** | Cron / Webhook 定时触发,把日报、周报、巡检交给 agent 跑 |
| **可复用技能** | 解决方案沉淀为 Skill,跨 agent、跨团队复用 |
| **多 Runtime** | 本地 daemon + 云端 runtime,统一控制台管理算力 |

---

## 并行 / 串行协作:如何配置

### 1. 并行协作:Squad 分发

把任务分给 Squad,leader 会**同时**把子任务派给多个 specialist:

```bash
# 创建 Squad,指定 leader
multica squad create --name "Frontend Team" --leader frontend-lead-agent

# 添加 specialist members
multica squad member add <squad-id> --member-id <agent-uuid> --type agent --role "负责 Tailwind / shadcn"
multica squad member add <squad-id> --member-id <agent-uuid> --type agent --role "最终 review"
```

把 issue 分给 Squad 后,流程是:

1. Leader 接收任务,读取问题内容
2. Leader 在评论里 `@` 多个 specialist(每个 `@` 触发**独立任务**)
3. 各 specialist **并行**执行,完成后回到 issue 反馈
4. Leader 看到结果后,可再次派发下一轮、升级、或停止

> Squad 本身只是个**路由层**——不增加能力,只解决"该分给谁"。

### 2. 串行协作:评论串联

在 issue 评论里 `@` 不同的 agent,串成一个工作流:

```
User 发 issue ─→ @researcher 调研 ─→ 评论触发 @implementer ─→ @reviewer 验收 ─→ 完成
```

每次 `@` 都会触发**该 agent 的新一轮任务**,并把整个 issue 的上下文(原 issue 描述 + 历史评论)传过去。前一个 agent 的输出自然成为后一个 agent 的输入。

### 3. 协作模式速查

| 场景 | 推荐配置 |
|------|----------|
| 一类问题有多个专家,不确定该谁接 | **Squad 并行**:leader 按内容分发 |
| 任务有明确上下游(A → B → C) | **串行 @mention**:评论里逐个触发 |
| 同一问题需要多视角同时分析 | **并行 @mention**:一条评论 `@` 多个 agent |
| 定时重复工作 | **Autopilot**:Cron 触发,自动派给指定 agent |
| 团队扩容,不想改任务分配方式 | **Squad**:成员变更不影响外部路由 |

---

## 高效运行:任务系统如何保证

每个 agent 执行的任务都经过完整的状态机:

```
queued ──daemon 拉取──> dispatched ──agent 启动──> running
                                                      │
                                          ┌───────────┼───────────┐
                                          ▼           ▼           ▼
                                     completed     failed ─重试─> queued
                                                      │
                                                  用户取消
                                                      ▼
                                                  cancelled
```

**保证高效运行的关键机制:**

- **队列 + 30 秒扫描** —— 服务端持续扫描任务状态,异常任务快速发现
- **自动重试(最多 2 次)** —— `timeout` / `runtime_offline` / `runtime_recovery` 三种故障自动重跑,**不重试** `agent_error`(API 报错、配额超限等底层问题)
- **会话恢复** —— 自动重试时复用 session,基础设施故障不会让 agent 从零开始
- **手动重跑强制新 session** —— 手动 rerun 总是开新会话,避免"上一次的错误状态"被带回来
- **失败回滚状态** —— agent 任务失败后,issue 状态自动从 `in_progress` 回退到 `todo`,看板上一眼可见
- **去重保护** —— 同一 leader 已有 queued/dispatched 任务时,新触发不会重复入队,避免自激振荡

> 详细说明见 [Tasks 文档](apps/docs/content/docs/tasks.mdx)。

---

## 快速开始

### 安装 CLI

```bash
# macOS / Linux(推荐)
brew install multica-ai/tap/multica

# 或一键脚本
curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash
```

### 三步跑起来

```bash
# 1. 配置 + 登录 + 启动 daemon
multica setup

# 2. 创建一个 agent(在 Web 端:设置 → Agents → 新建 Agent)

# 3. 创建 issue 并分配给 agent,看它自动执行
multica issue create --title "修复登录 Bug" --assignee my-agent
```

daemon 会自动检测 PATH 中的 AI 编码工具(`claude`、`codex`、`copilot` 等),并把任务路由到对应 agent。

### 自部署完整服务

```bash
curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --with-server
multica setup self-host
```

需要 Docker。详见 [自部署指南](SELF_HOSTING.md)。

---

## 架构

```
┌──────────────┐     ┌──────────────┐     ┌──────────────────┐
│   Next.js    │────>│  Go 后端     │────>│   PostgreSQL     │
│   前端       │<────│  (Chi + WS)  │<────│   (pgvector)     │
└──────────────┘     └──────┬───────┘     └──────────────────┘
                            │
                     ┌──────┴───────┐
                     │ Agent Daemon │  本地 / 云端 Runtime
                     └──────────────┘  执行 12 种 AI 编码工具
```

| 层 | 技术栈 |
|------|--------|
| 前端 | Next.js 16 (App Router) |
| 后端 | Go (Chi, sqlc, gorilla/websocket) |
| 数据库 | PostgreSQL 17 + pgvector |
| Agent Runtime | 本地 daemon,12 种 provider 可选 |

---

## 开发

贡献者请参阅 [贡献指南](CONTRIBUTING.md)。

```bash
make dev              # 自动检测环境、装依赖、建库、起服务
pnpm typecheck        # TypeScript 检查
make test             # Go 测试
```

**环境要求:** Node.js v20+、pnpm v10.28+、Go v1.26+、Docker

---

## 开源协议

[Modified Apache 2.0 (with commercial restrictions)](LICENSE)
