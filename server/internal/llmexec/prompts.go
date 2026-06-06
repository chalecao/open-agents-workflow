package llmexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// buildPrompts assembles the (system, user) prompt pair for a
// claimed task. The system prompt is the agent's instructions + a
// short skill summary; the user prompt is the issue / chat /
// autopilot text the user originally produced. The latter is the
// only piece that varies by task source — keeping the system
// prompt constant per agent means a fine-tuned LLM can hold
// per-agent behavior across runs.
func buildPrompts(
	ctx context.Context,
	queries *db.Queries,
	agent db.Agent,
	task *db.AgentTaskQueue,
) (systemPrompt, userPrompt string, err error) {
	var skills []db.Skill
	if rows, qerr := queries.ListAgentSkills(ctx, agent.ID); qerr == nil {
		skills = rows
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(agent.Instructions))
	if len(skills) > 0 {
		b.WriteString("\n\n## Available Skills\n")
		for _, s := range skills {
			name := strings.TrimSpace(s.Name)
			body := strings.TrimSpace(s.Content)
			if body == "" {
				continue
			}
			b.WriteString("\n### ")
			b.WriteString(name)
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	systemPrompt = b.String()

	// User prompt resolution: prefer the issue body, fall back to
	// the chat message, fall back to the autopilot trigger payload.
	// A task that has none of the three is treated as an error —
	// the user explicitly invoked the agent, so something must
	// have prompted it.
	switch {
	case task.IssueID.Valid:
		issue, ierr := queries.GetIssue(ctx, task.IssueID)
		if ierr != nil {
			return "", "", fmt.Errorf("load issue: %w", ierr)
		}
		var parts []string
		if title := strings.TrimSpace(issue.Title); title != "" {
			parts = append(parts, "# "+title)
		}
		if body := strings.TrimSpace(issue.Description.String); body != "" {
			parts = append(parts, body)
		}
		// A trigger comment is the most common entry point for
		// resumed tasks; surface it verbatim so the LLM sees the
		// user's exact words.
		if task.TriggerCommentID.Valid {
			if c, cerr := queries.GetComment(ctx, task.TriggerCommentID); cerr == nil {
				if t := strings.TrimSpace(c.Content); t != "" {
					parts = append(parts, "## Latest user comment\n"+t)
				}
			}
		}
		if len(parts) == 0 {
			return "", "", errors.New("issue has no body to send to the LLM")
		}
		userPrompt = strings.Join(parts, "\n\n")
	case task.ChatSessionID.Valid:
		cs, cerr := queries.GetChatSession(ctx, task.ChatSessionID)
		if cerr != nil {
			return "", "", fmt.Errorf("load chat session: %w", cerr)
		}
		// The task context JSONB carries the actual user message
		// the dispatcher batched. Fall back to the chat title when
		// missing.
		if task.Context != nil {
			var qc service.QuickCreateContext
			if json.Unmarshal(task.Context, &qc) == nil && strings.TrimSpace(qc.Prompt) != "" {
				userPrompt = qc.Prompt
				break
			}
		}
		userPrompt = strings.TrimSpace(cs.Title)
	case task.AutopilotRunID.Valid:
		run, rerr := queries.GetAutopilotRun(ctx, task.AutopilotRunID)
		if rerr != nil {
			return "", "", fmt.Errorf("load autopilot run: %w", rerr)
		}
		if run.TriggerPayload != nil {
			userPrompt = string(run.TriggerPayload)
		} else {
			ap, _ := queries.GetAutopilot(ctx, run.AutopilotID)
			if ap.Description.Valid {
				userPrompt = ap.Description.String
			}
		}
	default:
		// Quick-create path: the prompt is in task.context.
		if task.Context != nil {
			var qc service.QuickCreateContext
			if json.Unmarshal(task.Context, &qc) == nil && strings.TrimSpace(qc.Prompt) != "" {
				userPrompt = qc.Prompt
				break
			}
		}
		return "", "", errors.New("task has no resolvable user prompt")
	}
	if strings.TrimSpace(userPrompt) == "" {
		return "", "", errors.New("task user prompt is empty")
	}
	return systemPrompt, userPrompt, nil
}

// _ is a guard for an import-only ref so the linter doesn't drop
// pgtype. The runtime IDs in this file are pgtype.UUID; the
// uuidString helper above is the only place we serialize them.
var _ = pgtype.UUID{}
