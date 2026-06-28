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
  WorktreeFileResponse,
  WorktreeListItem,
  WorktreeTreeFile,
  WorktreeTreeResponse,
} from "@multica/core/api/schemas";
import { useT } from "../../i18n";
import { CodeBlock } from "@multica/ui/markdown/CodeBlock";

// ============================================================================
// WorktreeListSection
// ============================================================================
//
// Renders the per-issue worktree sidebar (right panel, below the execution
// log). One row per worktree, with the following lifecycle:
//
//   1. list endpoint → list of worktree summary rows (DB only, no FS).
//      Always cheap, no body shown until expanded.
//   2. expand → lazy tree endpoint per worktree. While the file tree is
//      in flight the row shows a spinner; on error the row inlines the
//      failure message.
//   3. expanded body is a true nested directory tree (every tracked +
//      untracked non-gitignored file in the worktree). Changed files
//      carry a status badge + +/- counts. Clicking a file opens the
//      existing before/after content dialog.
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

  // File tree is lazy — only fetched the first time the row is expanded,
  // and cached in React Query so flipping expand/collapse is free. The
  // endpoint runs `git ls-files` + `git ls-files --others
  // --exclude-standard` on the daemon host; expected to be < 1s for
  // typical worktrees, but the spinner stays put to keep the affordance
  // honest on huge monorepos.
  const treeQuery = useQuery({
    queryKey: ["issues", "worktree-tree", issueId, worktree.id] as const,
    queryFn: () => api.listIssueWorktreeTree(issueId, worktree.id),
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
  // `worktree.id` IS the absolute path. The row header truncates it so
  // the row stays compact, but the full path must stay one hover away —
  // the copy button already gives that affordance, but a Tooltip on the
  // title text is a faster "what is this?" signal and means the user
  // does not have to find and click the copy button just to see the
  // path. Skip the Tooltip on short ids to avoid a noisy hover target
  // for in-tree worktrees like `/tmp/wt-abc`.
  const showFullPathTooltip = worktree.id.length > shortId.length;
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
              {showFullPathTooltip ? (
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <span className="truncate text-xs font-medium">
                        {shortId}
                      </span>
                    }
                  />
                  <TooltipContent side="top" className="font-mono text-xs">
                    {worktree.id}
                  </TooltipContent>
                </Tooltip>
              ) : (
                <span className="truncate text-xs font-medium">
                  {shortId}
                </span>
              )}
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
          {treeQuery.isLoading && (
            <div className="flex items-center gap-1 text-[10px] text-muted-foreground px-1">
              <Loader2 className="h-3 w-3 animate-spin" />
              {t(($) => $.worktree_sidebar.tree_load_pending)}
            </div>
          )}
          {treeQuery.error && (
            <div className="flex items-center gap-1 text-[10px] text-destructive px-1">
              <AlertTriangle className="h-3 w-3" />
              {t(($) => $.worktree_sidebar.tree_load_failed)}
            </div>
          )}
          {treeQuery.data && (
            <TreeBody
              tree={treeQuery.data}
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
// Tree body — full worktree directory tree
// ============================================================================
//
// Replaces the old diff-only body. Walks the flat list returned by
// /worktrees/:id/tree and folds it into a real nested directory tree so
// the user can browse any file in the worktree, not just the ones that
// changed. The structure is intentionally shallow-by-default: the root
// level is open on first render, every subdirectory starts open too, and
// the user can collapse any branch by clicking the chevron.
//
// The tree node map is rebuilt via useMemo whenever the underlying file
// list changes (i.e. on a refetch). Memoization keeps the directory
// buckets referentially stable across re-renders that don't change
// `files`, which keeps child rows from re-rendering on unrelated parent
// state changes (e.g. comment-card reactions triggering a top-level
// re-render).

type TreeNode = {
  /** Display name — last path segment. */
  name: string;
  /** Absolute path from worktree root, including this segment. */
  path: string;
  /** Direct children. Empty for leaf files. */
  children: TreeNode[];
  /** Set on file leaves. Undefined for directory nodes. */
  file?: WorktreeTreeFile;
  /** True when the node represents a directory (not a file). */
  isDir: boolean;
};

/**
 * Build a nested tree from the flat file list. The empty path "" is
 * the root pseudo-node; every file with a non-empty path lands at the
 * matching depth. Files with no slash (e.g. "README.md") are direct
 * children of the root.
 *
 * Stable order: siblings are sorted alphabetically (case-insensitive)
 * with directories first so a directory and its contents group
 * together when the user opens the parent. Files that share the same
 * name as a sibling directory would be ambiguous, but that can't
 * happen on a real filesystem.
 */
function buildTree(files: WorktreeTreeFile[]): TreeNode {
  const root: TreeNode = { name: "", path: "", children: [], isDir: true };
  for (const file of files) {
    const segments = file.path.split("/").filter(Boolean);
    if (segments.length === 0) continue;
    let current = root;
    let acc = "";
    for (let i = 0; i < segments.length; i++) {
      const seg = segments[i]!;
      acc = acc ? `${acc}/${seg}` : seg;
      const isLeaf = i === segments.length - 1;
      let child = current.children.find((c) => c.name === seg && c.isDir === !isLeaf);
      if (!child) {
        child = {
          name: seg,
          path: acc,
          children: [],
          isDir: !isLeaf,
        };
        if (isLeaf) child.file = file;
        current.children.push(child);
      } else if (isLeaf) {
        // The path is a tracked file AND there's already a directory
        // entry of the same name (impossible on a real filesystem but
        // cheap to guard against). Prefer the file entry.
        child.file = file;
      }
      current = child;
    }
  }
  sortTree(root);
  return root;
}

function sortTree(node: TreeNode): void {
  node.children.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
  });
  for (const child of node.children) sortTree(child);
}

/** Count of file leaves in a subtree (used for the per-folder count chip). */
function countFiles(node: TreeNode): number {
  if (!node.isDir) return 1;
  let n = 0;
  for (const child of node.children) n += countFiles(child);
  return n;
}

/** Count of changed file leaves in a subtree (used for the change-count chip). */
function countChanged(node: TreeNode): number {
  if (!node.isDir) return node.file && node.file.status ? 1 : 0;
  let n = 0;
  for (const child of node.children) n += countChanged(child);
  return n;
}

function TreeBody({
  tree,
  onFileClick,
}: {
  tree: WorktreeTreeResponse;
  onFileClick: (path: string) => void;
}) {
  const { t } = useT("issues");
  const files = tree.files ?? [];

  const root = useMemo(() => buildTree(files), [files]);

  if (files.length === 0) {
    return (
      <p className="px-1 text-[10px] text-muted-foreground">
        {t(($) => $.worktree_sidebar.files_empty)}
      </p>
    );
  }

  const totalFiles = files.length;
  const changedCount = files.filter((f) => !!f.status).length;
  const truncated = tree.truncated;
  const totalCount = tree.total_count;

  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-1 px-1 text-[10px] font-medium text-muted-foreground uppercase tracking-wide">
        {t(($) => $.worktree_sidebar.files_section)}
        <span className="font-mono tabular-nums">
          {t(($) => $.worktree_sidebar.files_total, {
            shown: totalFiles,
            changed: changedCount,
          })}
        </span>
      </div>
      {truncated && (
        <p className="px-1 text-[10px] text-warning">
          {t(($) => $.worktree_sidebar.tree_truncated, {
            shown: totalFiles,
            total: totalCount,
          })}
        </p>
      )}
      <ul className="space-y-0.5">
        {root.children.map((child) => (
          <TreeRow
            key={child.path || child.name}
            node={child}
            depth={0}
            onFileClick={onFileClick}
          />
        ))}
      </ul>
    </div>
  );
}

function TreeRow({
  node,
  depth,
  onFileClick,
}: {
  node: TreeNode;
  depth: number;
  onFileClick: (path: string) => void;
}) {
  const { t } = useT("issues");
  // Per-directory open/closed state. Falsy key means "use default" —
  // the default is open at the root level (so a shallow tree unfolds
  // on first paint) and closed below depth 1 (so a 200-file monorepo
  // does not whitewash the sidebar with everything expanded).
  const [openDirs, setOpenDirs] = useState<Record<string, boolean>>({});
  const isOpen = openDirs[node.path] ?? depth < 1;
  const fileCount = countFiles(node);
  const changedInNode = countChanged(node);

  if (!node.isDir) {
    return (
      <li>
        <TreeFileRow file={node.file!} onClick={() => onFileClick(node.path)} />
      </li>
    );
  }

  return (
    <li>
      <button
        type="button"
        onClick={() =>
          setOpenDirs((prev) => ({ ...prev, [node.path]: !isOpen }))
        }
        className="flex w-full items-center gap-1 rounded px-1 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/40 hover:text-foreground"
        aria-label={
          isOpen
            ? t(($) => $.worktree_sidebar.collapse_dir_aria, { path: node.path })
            : t(($) => $.worktree_sidebar.expand_dir_aria, { path: node.path })
        }
      >
        <ChevronRight
          className={cn(
            "!size-2.5 shrink-0 stroke-[2.5] transition-transform",
            isOpen ? "rotate-90" : "",
          )}
        />
        <Folder className="h-3 w-3 shrink-0" />
        <span className="truncate font-mono">{node.name || "/"}</span>
        <span className="ml-auto inline-flex items-center gap-1 font-mono tabular-nums text-[10px]">
          {changedInNode > 0 && (
            <span className="rounded bg-accent px-1 text-foreground/80">
              {changedInNode}
            </span>
          )}
          <span>{fileCount}</span>
        </span>
      </button>
      {isOpen && (
        <ul
          className="ml-3 mt-0.5 space-y-0.5 border-l border-border/40 pl-2"
          // Each nesting level adds 12px of left padding via ml-3. That
          // is a fine balance between "deep paths get pushed off the
          // sidebar" and "two levels deep is invisible". The CSS is on
          // the parent <ul> so the indent applies to every child
          // uniformly.
        >
          {node.children.map((child) => (
            <TreeRow
              key={child.path || child.name}
              node={child}
              depth={depth + 1}
              onFileClick={onFileClick}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

function TreeFileRow({
  file,
  onClick,
}: {
  file: WorktreeTreeFile;
  onClick: () => void;
}) {
  const { t } = useT("issues");
  // `git status --porcelain` is two characters: index status then
  // worktree status. The sidebar only cares about the visible effect
  // ("did the user touch this file?"), so we fold both columns down
  // to a single-letter badge.
  const effectiveStatus = deriveFileStatus(file.status);

  const Icon =
    file.binary
      ? File
      : effectiveStatus === "A" || effectiveStatus === "??"
        ? FilePlus2
        : effectiveStatus === "D"
          ? FileMinus2
          : effectiveStatus === "R" || effectiveStatus === "C"
            ? Pencil
            : FileCode2;

  const statusLabel = (() => {
    if (!effectiveStatus) return null;
    switch (effectiveStatus) {
      case "A": return t(($) => $.worktree_sidebar.file_status_added);
      case "D": return t(($) => $.worktree_sidebar.file_status_deleted);
      case "R": return t(($) => $.worktree_sidebar.file_status_renamed);
      case "??": return t(($) => $.worktree_sidebar.untracked);
      default: return t(($) => $.worktree_sidebar.file_status_unknown);
    }
  })();

  return (
    <li>
      <Tooltip>
        <TooltipTrigger
          render={
            <button
              type="button"
              onClick={onClick}
              className="flex w-full items-center gap-1.5 rounded px-1 py-0.5 text-[11px] text-foreground hover:bg-accent/40"
            >
              <Icon className="h-3 w-3 shrink-0 text-muted-foreground" />
              <span className="min-w-0 flex-1 truncate font-mono text-left">
                {basename(file.path)}
              </span>
              {statusLabel && (
                <span className="shrink-0 text-[10px] uppercase tracking-wide text-muted-foreground">
                  {statusLabel}
                </span>
              )}
              {!file.binary && (file.additions > 0 || file.deletions > 0) && (
                <span className="shrink-0 font-mono tabular-nums text-[10px]">
                  <span className="text-success">+{file.additions}</span>
                  <span className="mx-0.5 text-muted-foreground">/</span>
                  <span className="text-destructive">-{file.deletions}</span>
                </span>
              )}
            </button>
          }
        />
        {/* Full path on hover — file rows only show the basename because
            the directory tree already conveys the path, but a Tooltip
            on each row makes copy-by-screenshot easy without the user
            having to expand the tree to its deepest level. */}
        <TooltipContent side="right" className="font-mono text-xs">
          {file.path}
        </TooltipContent>
      </Tooltip>
    </li>
  );
}

/**
 * Collapse the two-column porcelain status into a single visible badge
 * letter. Index and worktree columns are independent, so we pick the
 * one that signals user action — anything in column 1 means the file
 * is staged (intent-to-add counts), anything in column 2 means it was
 * touched in the working tree. Renames/copies span both columns, so
 * they show up as "R" / "C" regardless.
 */
function deriveFileStatus(status: string): string {
  if (!status) return "";
  if (status === "??") return "??";
  if (status[0] === "R" || status[0] === "C") return "R";
  if (status[1] === "R" || status[1] === "C") return "R";
  if (status[0] === "A") return "A";
  if (status[1] === "A") return "A";
  if (status[0] === "D" || status[1] === "D") return "D";
  if (status[0] === "M" || status[1] === "M") return "M";
  return status;
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
      <DialogContent className="!max-w-6xl !w-[90vw] max-h-[85vh] flex flex-col">
        <DialogHeader>
          <DialogTitle className="font-mono text-sm">
            {path && t(($) => $.worktree_sidebar.file_dialog_title, { path })}
          </DialogTitle>
          <DialogDescription className="text-xs text-muted-foreground">
            {query.data && <FileSubtitle data={query.data} />}
          </DialogDescription>
        </DialogHeader>
        <div className="flex-1 min-h-0 overflow-auto">
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

  // Detect language from file path extension
  const language = getFileLanguage(data.path);

  // Two-pane side-by-side diff: before on the left, after on the right.
  // Empty before/after cells render a muted "no content" placeholder so
  // the user can tell which side is missing at a glance.
  return (
    <div className="rounded border border-border/40">
      <div className="grid grid-cols-2 divide-x divide-border/40">
        <div className="min-h-[200px]">
          {data.truncated && (
            <div className="border-b border-border/40 bg-warning/10 px-3 py-1 text-[10px] text-warning">
              {t(($) => $.worktree_sidebar.file_dialog_truncated)}
            </div>
          )}
          {data.before ? (
            <CodeBlock code={data.before} language={language} mode="minimal" />
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
            <CodeBlock code={data.after} language={language} mode="minimal" />
          ) : (
            <div className="flex h-full items-center justify-center p-4 text-[11px] text-muted-foreground">
              {t(($) => $.worktree_sidebar.file_dialog_no_after)}
            </div>
          )}
        </div>
      </div>
    </div>
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

// Language detection map from file extension
const EXT_TO_LANGUAGE: Record<string, string> = {
  js: "javascript",
  jsx: "javascript",
  ts: "typescript",
  tsx: "typescript",
  mjs: "javascript",
  cjs: "javascript",
  py: "python",
  rb: "ruby",
  go: "go",
  rs: "rust",
  java: "java",
  kt: "kotlin",
  swift: "swift",
  cs: "csharp",
  cpp: "cpp",
  cc: "cpp",
  cxx: "cpp",
  c: "c",
  h: "c",
  hpp: "cpp",
  php: "php",
  ruby: "ruby",
  yaml: "yaml",
  yml: "yaml",
  json: "json",
  jsonc: "json",
  xml: "xml",
  html: "html",
  htm: "html",
  css: "css",
  scss: "scss",
  sass: "sass",
  less: "less",
  md: "markdown",
  markdown: "markdown",
  sql: "sql",
  sh: "bash",
  bash: "bash",
  zsh: "bash",
  fish: "bash",
  ps1: "powershell",
  psd1: "powershell",
  psm1: "powershell",
  dockerfile: "dockerfile",
  makefile: "makefile",
  toml: "toml",
  ini: "ini",
  cfg: "ini",
  conf: "ini",
  tf: "hcl",
  hcl: "hcl",
  vue: "vue",
  svelte: "svelte",
  svg: "svg",
  png: "text",
  jpg: "text",
  jpeg: "text",
  gif: "text",
  webp: "text",
  ico: "text",
  pdf: "text",
  zip: "text",
  gz: "text",
  tar: "text",
};

/**
 * Detect language from file path for syntax highlighting.
 * Uses the file extension to determine the language.
 */
function getFileLanguage(path: string): string {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  return EXT_TO_LANGUAGE[ext] ?? "text";
}
