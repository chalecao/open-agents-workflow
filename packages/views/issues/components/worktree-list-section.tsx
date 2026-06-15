"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  AlertTriangle,
  ChevronRight,
  Copy,
  File,
  FileCode2,
  FileMinus2,
  FilePlus2,
  Folder,
  GitBranch,
  GitCommit,
  Loader2,
  Pencil,
} from "lucide-react";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@multica/ui/components/ui/dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@multica/ui/components/ui/tooltip";
import { copyText } from "@multica/ui/lib/clipboard";
import { cn } from "@multica/ui/lib/utils";
import { api } from "@multica/core/api";
import type {
  WorktreeDiffResponse,
  WorktreeFileChange,
  WorktreeFileResponse,
  WorktreeListItem,
} from "@multica/core/api/schemas";
import { useT } from "../../i18n";
import { ScrollArea } from "@multica/ui/components/ui/scroll-area";

// ============================================================================
// WorktreeListSection
// ============================================================================
//
// Renders the per-issue worktree sidebar (right panel, below the execution
// log). One row per worktree, with the following lifecycle:
//
//   1. list endpoint → list of worktree summary rows (DB only, no FS).
//      Always cheap, no body shown until expanded.
//   2. expand → lazy diff endpoint per worktree. While the diff is in
//      flight the row shows a spinner; on error the row inlines the
//      failure message.
//   3. expanded body groups changed files by directory; clicking a file
//      opens a dialog with before/after content.
//
// All requests are routed through the api client and cached by React
// Query. Cross-link state (which worktree is currently selected from an
// agent card) is passed in via the `selectedWorktreeId` prop — when set,
// the row auto-expands and scrolls into view. See WorktreeListSection
// for how that wires up to CommentRow clicks.

interface WorktreeListSectionProps {
  issueId: string;
  /** Worktree id (== absolute path) currently pinned by an agent card click. */
  selectedWorktreeId?: string | null;
}

export function WorktreeListSection({
  issueId,
  selectedWorktreeId,
}: WorktreeListSectionProps) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(true);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const { data, isLoading, error } = useQuery({
    queryKey: ["issues", "worktrees", issueId] as const,
    queryFn: () => api.listIssueWorktrees(issueId),
    staleTime: 30_000,
  });

  const worktrees = data ?? [];

  // selectedWorktreeId is the id from the agent card; auto-expand the
  // matching row so the user immediately sees the diff it points to.
  // Toggling the prop off does not collapse the row (idempotent — the
  // user can collapse manually).
  const effectiveExpanded = useMemo<Record<string, boolean>>(() => {
    if (!selectedWorktreeId) return expanded;
    return { ...expanded, [selectedWorktreeId]: true };
  }, [expanded, selectedWorktreeId]);

  if (isLoading) {
    return (
      <div className="px-2 py-1 text-xs text-muted-foreground inline-flex items-center gap-1">
        <Loader2 className="h-3 w-3 animate-spin" />
        {t(($) => $.worktree_sidebar.section)}
      </div>
    );
  }
  if (error) {
    return (
      <div className="px-2 py-1 text-xs text-destructive">
        {t(($) => $.worktree_sidebar.worktree_load_failed)}
      </div>
    );
  }
  if (worktrees.length === 0) return null;

  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen(!open)}
        className={cn(
          "flex w-full items-center gap-1 rounded-md px-2 py-1 text-xs font-medium transition-colors mb-2",
          "hover:bg-accent/70",
          open ? "" : "text-muted-foreground hover:text-foreground",
        )}
      >
        {t(($) => $.worktree_sidebar.section)}
        <ChevronRight
          className={cn(
            "!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform",
            open ? "rotate-90" : "",
          )}
        />
        <span className="ml-auto font-mono tabular-nums text-muted-foreground">
          {worktrees.length}
        </span>
      </button>
      {open && (
        <div className="space-y-1 pl-2">
          {worktrees.map((w) => (
            <WorktreeRow
              key={w.id}
              worktree={w}
              issueId={issueId}
              expanded={!!effectiveExpanded[w.id]}
              onToggle={() =>
                setExpanded((prev) => ({ ...prev, [w.id]: !prev[w.id] }))
              }
            />
          ))}
        </div>
      )}
    </div>
  );
}

// ============================================================================
// Worktree row (header + expandable diff body)
// ============================================================================

function WorktreeRow({
  worktree,
  issueId,
  expanded,
  onToggle,
}: {
  worktree: WorktreeListItem;
  issueId: string;
  expanded: boolean;
  onToggle: () => void;
}) {
  const { t } = useT("issues");
  const [fileOpen, setFileOpen] = useState<{
    path: string;
  } | null>(null);

  // Diff is lazy — only fetched the first time the row is expanded, and
  // cached in React Query so flipping expand/collapse is free. The
  // endpoint hits `git diff` on the daemon host; expected to be < 1s
  // for typical worktrees, but the spinner is left in place to keep
  // the affordance honest on big refactors.
  const diffQuery = useQuery({
    queryKey: ["issues", "worktree-diff", issueId, worktree.id] as const,
    queryFn: () => api.getIssueWorktreeDiff(issueId, worktree.id),
    enabled: expanded,
    staleTime: 60_000,
  });

  const branchLabel =
    worktree.branch && worktree.base_branch
      ? t(($) => $.worktree_sidebar.branch_badge, {
          branch: worktree.branch,
          base: worktree.base_branch,
        })
      : worktree.branch
        ? t(($) => $.worktree_sidebar.branch_badge_no_base, {
            branch: worktree.branch,
          })
        : t(($) => $.worktree_sidebar.branch_unknown);

  const shortId = useShortPath(worktree.id);
  const taskCount = worktree.task_count;
  const commentCount = worktree.comment_count;
  const isGCd = !worktree.exists;

  return (
    <div
      data-worktree-id={worktree.id}
      className="rounded-md border border-border/40 bg-card/30"
    >
      <div className="flex items-center gap-1.5 px-1.5 py-1.5">
        <button
          type="button"
          onClick={onToggle}
          aria-label={t(($) => $.worktree_sidebar.expand_tooltip)}
          className="flex items-center gap-1.5 flex-1 min-w-0 text-left"
        >
          <ChevronRight
            className={cn(
              "!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform",
              expanded ? "rotate-90" : "",
            )}
          />
          <GitBranch className="h-3 w-3 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-1 min-w-0">
              <span className="truncate text-xs font-medium">
                {shortId}
              </span>
              {isGCd && (
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <span className="shrink-0 text-[10px] text-muted-foreground">
                        {t(($) => $.worktree_sidebar.exists_no)}
                      </span>
                    }
                  />
                  <TooltipContent side="top">
                    {t(($) => $.worktree_sidebar.exists_no_tooltip)}
                  </TooltipContent>
                </Tooltip>
              )}
            </div>
            <div className="truncate text-[10px] text-muted-foreground">
              {branchLabel}
            </div>
          </div>
        </button>
        <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground shrink-0">
          {taskCount > 0 && (
            <span className="font-mono tabular-nums">
              {t(($) => $.worktree_sidebar.task_count, { count: taskCount })}
            </span>
          )}
          {commentCount > 0 && (
            <span className="font-mono tabular-nums">
              {t(($) => $.worktree_sidebar.comment_count, { count: commentCount })}
            </span>
          )}
          <Tooltip>
            <TooltipTrigger
              render={
                <button
                  type="button"
                  onClick={() => {
                    void copyText(worktree.id).then((ok) => {
                      if (ok) toast.success(t(($) => $.worktree_sidebar.copied_toast));
                    });
                  }}
                  aria-label={t(($) => $.worktree_sidebar.copy_path_action)}
                  className="flex items-center justify-center rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground transition-colors"
                >
                  <Copy className="h-3 w-3" />
                </button>
              }
            />
            <TooltipContent side="top">
              {t(($) => $.worktree_sidebar.copy_path_action)}
            </TooltipContent>
          </Tooltip>
        </div>
      </div>

      {expanded && (
        <div className="border-t border-border/40 px-1.5 py-1.5">
          {diffQuery.isLoading && (
            <div className="flex items-center gap-1 text-[10px] text-muted-foreground px-1">
              <Loader2 className="h-3 w-3 animate-spin" />
              {t(($) => $.worktree_sidebar.diff_load_failed)}
            </div>
          )}
          {diffQuery.error && (
            <div className="flex items-center gap-1 text-[10px] text-destructive px-1">
              <AlertTriangle className="h-3 w-3" />
              {t(($) => $.worktree_sidebar.diff_load_failed)}
            </div>
          )}
          {diffQuery.data && (
            <DiffBody
              diff={diffQuery.data}
              onFileClick={(path) => setFileOpen({ path })}
            />
          )}
        </div>
      )}

      <WorktreeFileDialog
        issueId={issueId}
        worktreeId={worktree.id}
        file={fileOpen}
        onClose={() => setFileOpen(null)}
      />
    </div>
  );
}

// ============================================================================
// Diff body — file tree grouped by directory
// ============================================================================

function DiffBody({
  diff,
  onFileClick,
}: {
  diff: WorktreeDiffResponse;
  onFileClick: (path: string) => void;
}) {
  const { t } = useT("issues");
  const files = diff.files ?? [];
  const unstaged = diff.unstaged_files ?? [];
  const untracked = diff.untracked ?? [];

  // Group files by top-level directory for the file tree. The grouping
  // is intentionally shallow (top-level dir + filename) so the tree
  // stays scannable when a worktree touches many subdirs; the user can
  // eyeball the changes at a glance. Memoized so the directory buckets
  // stay referentially stable across re-renders that don't change
  // `files` — keeps child FileRow components from re-rendering on
  // unrelated parent state changes.
  const grouped = useMemo(() => groupByDirectory(files), [files]);

  if (files.length === 0 && unstaged.length === 0 && untracked.length === 0) {
    return (
      <p className="px-1 text-[10px] text-muted-foreground">
        {t(($) => $.worktree_sidebar.files_empty)}
      </p>
    );
  }

  return (
    <div className="space-y-1.5">
      {diff.diff_truncated && (
        <p className="px-1 text-[10px] text-warning">
          {t(($) => $.worktree_sidebar.diff_truncated)}
        </p>
      )}
      {files.length > 0 && (
        <div>
          <div className="flex items-center gap-1 px-1 mb-0.5 text-[10px] font-medium text-muted-foreground uppercase tracking-wide">
            {t(($) => $.worktree_sidebar.files_section)}
            <span className="font-mono tabular-nums">{files.length}</span>
          </div>
          <DirectoryTree
            grouped={grouped}
            onFileClick={onFileClick}
            emptyLabel={t(($) => $.worktree_sidebar.files_empty)}
          />
        </div>
      )}

      {(unstaged.length > 0 || untracked.length > 0) && (
        <div>
          <div className="flex items-center gap-1 px-1 mb-0.5 text-[10px] font-medium text-muted-foreground uppercase tracking-wide">
            {t(($) => $.worktree_sidebar.unstaged_section)}
            <span className="font-mono tabular-nums">
              {t(($) => $.worktree_sidebar.unstaged_files, {
                count: unstaged.length + untracked.length,
              })}
            </span>
          </div>
          <DirectoryTree
            grouped={groupByDirectory(unstaged)}
            onFileClick={onFileClick}
            untrackedPaths={untracked}
            emptyLabel={t(($) => $.worktree_sidebar.files_empty)}
          />
        </div>
      )}
    </div>
  );
}

// Group changes into { "src" → [WorktreeFileChange, ...], "src/foo" → [...], ... }
// The DirectoryTree renderer folds these into a single, nested list. Top-level
// entries with no slash ("README.md") live under the "root" bucket.
function groupByDirectory(
  files: WorktreeFileChange[],
): Map<string, WorktreeFileChange[]> {
  const out = new Map<string, WorktreeFileChange[]>();
  for (const f of files) {
    const idx = f.path.lastIndexOf("/");
    const dir = idx >= 0 ? f.path.slice(0, idx) : "root";
    const list = out.get(dir);
    if (list) list.push(f);
    else out.set(dir, [f]);
  }
  return out;
}

function DirectoryTree({
  grouped,
  untrackedPaths,
  onFileClick,
  emptyLabel,
}: {
  grouped: Map<string, WorktreeFileChange[]>;
  untrackedPaths?: string[];
  onFileClick: (path: string) => void;
  emptyLabel: string;
}) {
  const { t } = useT("issues");
  const [openDirs, setOpenDirs] = useState<Record<string, boolean>>({});

  if (grouped.size === 0 && (!untrackedPaths || untrackedPaths.length === 0)) {
    return <p className="px-1 text-[10px] text-muted-foreground">{emptyLabel}</p>;
  }

  return (
    <ul className="space-y-0.5">
      {[...grouped.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([dir, files]) => {
          const isOpen = openDirs[dir] ?? true;
          return (
            <li key={dir}>
              <button
                type="button"
                onClick={() =>
                  setOpenDirs((prev) => ({ ...prev, [dir]: !prev[dir] }))
                }
                className="flex w-full items-center gap-1 rounded px-1 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/40 hover:text-foreground"
              >
                <ChevronRight
                  className={cn(
                    "!size-2.5 shrink-0 stroke-[2.5] transition-transform",
                    isOpen ? "rotate-90" : "",
                  )}
                />
                <Folder className="h-3 w-3 shrink-0" />
                <span className="truncate font-mono">{dir === "root" ? "/" : dir}</span>
                <span className="ml-auto font-mono tabular-nums text-[10px]">
                  {files.length}
                </span>
              </button>
              {isOpen && (
                <ul className="ml-3 mt-0.5 space-y-0.5 border-l border-border/40 pl-2">
                  {files.map((f) => (
                    <FileRow key={f.path} file={f} onClick={() => onFileClick(f.path)} />
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      {untrackedPaths && untrackedPaths.length > 0 && (
        <li>
          <div className="flex items-center gap-1 rounded px-1 py-0.5 text-[11px] text-muted-foreground">
            <ChevronRight className="!size-2.5 shrink-0 stroke-[2.5] rotate-90" />
            <Folder className="h-3 w-3 shrink-0" />
            <span className="truncate font-mono">
              {t(($) => $.worktree_sidebar.untracked)}
            </span>
            <span className="ml-auto font-mono tabular-nums text-[10px]">
              {untrackedPaths.length}
            </span>
          </div>
          <ul className="ml-3 mt-0.5 space-y-0.5 border-l border-border/40 pl-2">
            {untrackedPaths.map((p) => (
              <FileRow
                key={p}
                file={{ path: p, status: "??", additions: 0, deletions: 0, binary: false }}
                onClick={() => onFileClick(p)}
              />
            ))}
          </ul>
        </li>
      )}
    </ul>
  );
}

function FileRow({
  file,
  onClick,
}: {
  file: WorktreeFileChange;
  onClick: () => void;
}) {
  const { t } = useT("issues");
  const Icon =
    file.binary
      ? File
      : file.status === "A" || file.status === "??"
        ? FilePlus2
        : file.status === "D"
          ? FileMinus2
          : file.status === "R" || file.status === "C"
            ? Pencil
            : file.status === "M"
              ? FileCode2
              : FileCode2;

  const statusLabel = (() => {
    switch (file.status) {
      case "A": return t(($) => $.worktree_sidebar.file_status_added);
      case "D": return t(($) => $.worktree_sidebar.file_status_deleted);
      case "R": return t(($) => $.worktree_sidebar.file_status_renamed);
      case "??": return t(($) => $.worktree_sidebar.untracked);
      default: return t(($) => $.worktree_sidebar.file_status_unknown);
    }
  })();

  return (
    <li>
      <button
        type="button"
        onClick={onClick}
        className="flex w-full items-center gap-1.5 rounded px-1 py-0.5 text-[11px] text-foreground hover:bg-accent/40"
      >
        <Icon className="h-3 w-3 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1 truncate font-mono text-left">
          {basename(file.path)}
        </span>
        <span className="shrink-0 text-[10px] uppercase tracking-wide text-muted-foreground">
          {statusLabel}
        </span>
        {!file.binary && (file.additions > 0 || file.deletions > 0) && (
          <span className="shrink-0 font-mono tabular-nums text-[10px]">
            <span className="text-success">+{file.additions}</span>
            <span className="mx-0.5 text-muted-foreground">/</span>
            <span className="text-destructive">-{file.deletions}</span>
          </span>
        )}
      </button>
    </li>
  );
}

function basename(path: string): string {
  const idx = path.lastIndexOf("/");
  return idx >= 0 ? path.slice(idx + 1) : path;
}

// ============================================================================
// File diff dialog
// ============================================================================

function WorktreeFileDialog({
  issueId,
  worktreeId,
  file,
  onClose,
}: {
  issueId: string;
  worktreeId: string;
  file: { path: string } | null;
  onClose: () => void;
}) {
  const { t } = useT("issues");
  const open = file !== null;
  const path = file?.path ?? "";

  const query = useQuery({
    queryKey: ["issues", "worktree-file", issueId, worktreeId, path] as const,
    queryFn: () => api.getIssueWorktreeFile(issueId, worktreeId, path),
    enabled: open && !!path,
    staleTime: 60_000,
  });

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-4xl max-h-[85vh] flex flex-col">
        <DialogHeader>
          <DialogTitle className="font-mono text-sm">
            {path && t(($) => $.worktree_sidebar.file_dialog_title, { path })}
          </DialogTitle>
          <DialogDescription className="text-xs text-muted-foreground">
            {query.data && <FileSubtitle data={query.data} />}
          </DialogDescription>
        </DialogHeader>
        <div className="flex-1 min-h-0 overflow-hidden">
          {query.isLoading && (
            <div className="flex items-center gap-2 px-2 py-4 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" />
              {t(($) => $.worktree_sidebar.worktree_load_failed)}
            </div>
          )}
          {query.error && (
            <div className="flex items-center gap-2 px-2 py-4 text-sm text-destructive">
              <AlertTriangle className="h-4 w-4" />
              {t(($) => $.worktree_sidebar.file_load_failed)}
            </div>
          )}
          {query.data && <FileBody data={query.data} />}
        </div>
        <div className="flex items-center justify-between border-t border-border/40 px-2 py-2 text-xs text-muted-foreground">
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t(($) => $.comment.cancel_action)}
          </Button>
          {query.data && (
            <div className="flex items-center gap-3 font-mono tabular-nums text-[10px]">
              <span>
                {t(($) => $.worktree_sidebar.file_size, {
                  bytes: query.data.before_bytes,
                })}
              </span>
              <span>→</span>
              <span>
                {t(($) => $.worktree_sidebar.file_size, {
                  bytes: query.data.after_bytes,
                })}
              </span>
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function FileSubtitle({ data }: { data: WorktreeFileResponse }) {
  const { t } = useT("issues");
  if (data.binary) {
    return (
      <span className="inline-flex items-center gap-1">
        <File className="h-3 w-3" />
        {t(($) => $.worktree_sidebar.file_binary)}
      </span>
    );
  }
  const isNew = !data.before && !!data.after;
  const isDeleted = !!data.before && !data.after;
  if (isNew) return <>{t(($) => $.worktree_sidebar.file_dialog_subtitle_new)}</>;
  if (isDeleted) return <>{t(($) => $.worktree_sidebar.file_dialog_subtitle_deleted)}</>;
  return (
    <span className="inline-flex items-center gap-1">
      <GitCommit className="h-3 w-3" />
      {data.base_sha
        ? t(($) => $.worktree_sidebar.file_dialog_subtitle_before, { sha: data.base_sha.slice(0, 8) })
        : "—"}
      <span className="text-muted-foreground">→</span>
      {data.head_sha
        ? t(($) => $.worktree_sidebar.file_dialog_subtitle_after, { sha: data.head_sha.slice(0, 8) })
        : "—"}
    </span>
  );
}

function FileBody({ data }: { data: WorktreeFileResponse }) {
  const { t } = useT("issues");
  if (data.binary) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-2 text-sm text-muted-foreground">
        <File className="h-8 w-8" />
        {t(($) => $.worktree_sidebar.file_binary)}
      </div>
    );
  }

  // Two-pane side-by-side diff: before on the left, after on the right.
  // Empty before/after cells render a muted "no content" placeholder so
  // the user can tell which side is missing at a glance.
  return (
    <ScrollArea className="h-full rounded border border-border/40">
      <div className="grid grid-cols-2 divide-x divide-border/40 font-mono text-xs">
        <div className="min-h-[200px]">
          {data.truncated && (
            <div className="border-b border-border/40 bg-warning/10 px-3 py-1 text-[10px] text-warning">
              {t(($) => $.worktree_sidebar.file_dialog_truncated)}
            </div>
          )}
          {data.before ? (
            <pre className="whitespace-pre-wrap break-all p-3 text-foreground/90">
              {data.before}
            </pre>
          ) : (
            <div className="flex h-full items-center justify-center p-4 text-[11px] text-muted-foreground">
              {t(($) => $.worktree_sidebar.file_dialog_no_before)}
            </div>
          )}
        </div>
        <div className="min-h-[200px]">
          {data.truncated && (
            <div className="border-b border-border/40 bg-warning/10 px-3 py-1 text-[10px] text-warning">
              {t(($) => $.worktree_sidebar.file_dialog_truncated)}
            </div>
          )}
          {data.after ? (
            <pre className="whitespace-pre-wrap break-all p-3 text-foreground/90">
              {data.after}
            </pre>
          ) : (
            <div className="flex h-full items-center justify-center p-4 text-[11px] text-muted-foreground">
              {t(($) => $.worktree_sidebar.file_dialog_no_after)}
            </div>
          )}
        </div>
      </div>
    </ScrollArea>
  );
}

// ============================================================================
// Helpers
// ============================================================================

// `id` is the absolute path; rendering the full path would dominate the
// sidebar row. Trim to the trailing 1–2 path components so the row
// stays compact while still being scannable across multiple worktrees
// (e.g. `…/wt-abc123`, `…/wt-def456`). On long paths the tooltip
// surfaces the full value via the title attr.
function useShortPath(id: string): string {
  if (id.length <= 40) return id;
  const parts = id.split("/").filter(Boolean);
  if (parts.length <= 2) return id;
  return `…/${parts.slice(-2).join("/")}`;
}
