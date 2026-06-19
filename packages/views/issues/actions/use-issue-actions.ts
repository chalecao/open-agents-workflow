"use client";

import { useCallback } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import type { Issue, UpdateIssueRequest } from "@multica/core/types";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { useModalStore } from "@multica/core/modals";
import { useUpdateIssue } from "@multica/core/issues/mutations";
import { pinListOptions, useCreatePin, useDeletePin } from "@multica/core/pins";
import { copyText } from "@multica/ui/lib/clipboard";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";

const BACKLOG_HINT_LS_KEY = "multica:backlog-agent-hint-dismissed";

export interface UseIssueActionsResult {
  isPinned: boolean;
  updateField: (updates: Partial<UpdateIssueRequest>) => void;
  togglePin: () => void;
  copyLink: () => Promise<void>;
  openCreateSubIssue: () => void;
  openSetParent: () => void;
  openAddChild: () => void;
  openDuplicate: () => void;
  openDeleteConfirm: (opts?: { onDeletedNavigateTo?: string }) => void;
}

/**
 * Accepts a nullable issue so callers can invoke the hook before they've
 * early-returned on a missing issue. Returned handlers are safe no-ops when
 * `issue` is null.
 */
export function useIssueActions(issue: Issue | null): UseIssueActionsResult {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const user = useAuthStore((s) => s.user);
  const userId = user?.id;

  const { data: pinnedItems = [] } = useQuery({
    ...pinListOptions(wsId, userId ?? ""),
    enabled: !!userId,
  });

  const isPinned =
    !!issue &&
    pinnedItems.some(
      (p) => p.item_type === "issue" && p.item_id === issue.id,
    );

  const updateIssue = useUpdateIssue();
  const createPin = useCreatePin();
  const deletePin = useDeletePin();
  const openModal = useModalStore((s) => s.open);

  const issueId = issue?.id ?? null;
  const issueStatus = issue?.status ?? null;
  const issueIdentifier = issue?.identifier ?? null;
  const issueProjectId = issue?.project_id ?? null;

  const updateField = useCallback(
    (updates: Partial<UpdateIssueRequest>) => {
      if (!issueId) return;
      updateIssue.mutate(
        { id: issueId, ...updates },
        {
          onError: (err) =>
            toast.error(
              err instanceof Error && err.message
                ? err.message
                : t(($) => $.detail.update_failed),
            ),
        },
      );
      // Hint: assigning an agent to a backlog issue won't trigger execution
      // until the issue is moved to an active status.
      if (
        updates.assignee_type === "agent" &&
        updates.assignee_id &&
        issueStatus === "backlog" &&
        typeof window !== "undefined" &&
        localStorage.getItem(BACKLOG_HINT_LS_KEY) !== "true"
      ) {
        openModal("issue-backlog-agent-hint", { issueId });
      }
    },
    [issueId, issueStatus, updateIssue, openModal, t],
  );

  const togglePin = useCallback(() => {
    if (!issueId) return;
    if (isPinned) {
      deletePin.mutate({ itemType: "issue", itemId: issueId });
    } else {
      createPin.mutate({ item_type: "issue", item_id: issueId });
    }
  }, [isPinned, issueId, createPin, deletePin]);

  const copyLink = useCallback(async () => {
    if (!issueId) return;
    const url = navigation.getShareableUrl(paths.issueDetail(issueId));
    if (await copyText(url)) {
      toast.success(t(($) => $.detail.link_copied));
    } else {
      toast.error(t(($) => $.detail.link_copy_failed));
    }
  }, [paths, issueId, navigation, t]);

  const openCreateSubIssue = useCallback(() => {
    if (!issueId) return;
    openModal("create-issue", {
      parent_issue_id: issueId,
      parent_issue_identifier: issueIdentifier,
      ...(issueProjectId ? { project_id: issueProjectId } : {}),
    });
  }, [openModal, issueId, issueIdentifier, issueProjectId]);

  const openSetParent = useCallback(() => {
    if (!issueId) return;
    openModal("issue-set-parent", { issueId });
  }, [openModal, issueId]);

  const openAddChild = useCallback(() => {
    if (!issueId) return;
    openModal("issue-add-child", { issueId });
  }, [openModal, issueId]);

  // Opens the create-issue modal pre-seeded with this issue's content so the
  // user can tweak the title and submit a duplicate. The new issue starts
  // fresh (no `parent_issue_id`) — duplicating preserves the content, not the
  // sub-issue relationship, which would create a confusing parent/child
  // graph when the user already has a "Set parent" action.
  const openDuplicate = useCallback(() => {
    if (!issue) return;
    openModal("create-issue", {
      // Snake-case to match the shape the create-issue panel already reads
      // for `data.*`; consistency with how `openCreateSubIssue` payloads are
      // assembled just above.
      title: issue.title,
      description: issue.description,
      status: issue.status,
      priority: issue.priority,
      assignee_type: issue.assignee_type,
      assignee_id: issue.assignee_id,
      start_date: issue.start_date,
      due_date: issue.due_date,
      project_id: issue.project_id,
      // Copy `handoff_data` only when non-empty — `{}` would round-trip as
      // "explicit empty" and clutters the dialog with a blank payload editor.
      ...(issue.handoff_data && Object.keys(issue.handoff_data).length > 0
        ? { handoff_data: issue.handoff_data }
        : {}),
    });
  }, [openModal, issue]);

  const openDeleteConfirm = useCallback(
    (opts?: { onDeletedNavigateTo?: string }) => {
      if (!issueId) return;
      openModal("issue-delete-confirm", {
        issueId,
        identifier: issueIdentifier,
        onDeletedNavigateTo: opts?.onDeletedNavigateTo,
      });
    },
    [openModal, issueId, issueIdentifier],
  );

  return {
    isPinned,
    updateField,
    togglePin,
    copyLink,
    openCreateSubIssue,
    openSetParent,
    openAddChild,
    openDuplicate,
    openDeleteConfirm,
  };
}
