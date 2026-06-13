"use client";

import { useEffect, useMemo, useState } from "react";
import { Braces, Check, X as XIcon } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Textarea } from "@multica/ui/components/ui/textarea";
import type { IssueHandoffData } from "@multica/core/types";
import { useT } from "../i18n";

// Size cap mirrors the server-side 8 KiB limit on `issue.handoff_data`.
// Keep this in sync with validateIssueHandoffData in the server handler.
const HANDOFF_DATA_MAX_BYTES = 8 * 1024;

type ParseErrorReason = "invalid_json" | "not_object" | "too_large";

type ParseResult =
  | { ok: true; value: IssueHandoffData }
  | { ok: false; reason: ParseErrorReason };

/**
 * Parse + validate a JSON string into an IssueHandoffData value.
 * Server enforces the same shape constraints; we mirror them here so the
 * user gets an inline error instead of a server round-trip on every save.
 */
function parseHandoffData(input: string): ParseResult {
  const trimmed = input.trim();
  if (trimmed.length === 0) {
    return { ok: true, value: {} };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return { ok: false, reason: "invalid_json" };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, reason: "not_object" };
  }
  if (trimmed.length > HANDOFF_DATA_MAX_BYTES) {
    return { ok: false, reason: "too_large" };
  }
  return { ok: true, value: parsed as IssueHandoffData };
}

function serializeHandoffData(value: IssueHandoffData | null | undefined): string {
  if (!value || Object.keys(value).length === 0) return "";
  return JSON.stringify(value, null, 2);
}

/**
 * HandoffDataDialog — JSON editor for `issue.handoff_data`. Renders inside
 * a Dialog so the user can edit large payloads without crowding the
 * surrounding form. Used from the create-issue dialog ("..." overflow) and
 * the issue-detail sidebar.
 *
 * - `value` is the *current* value (from the issue or the local form
 *   state); we initialize the textarea from it and reset on open.
 * - `onSave` is called with `null` when the user clears the field
 *   (empty textarea), and with the parsed object otherwise. We never
 *   pass an empty object — the server stores `{}` and `null` as the same
 *   default, but `null` is the explicit "clear" signal for the PATCH
 *   tri-state (so the column is overwritten, not left alone).
 */
export function HandoffDataDialog({
  open,
  onOpenChange,
  value,
  onSave,
  title,
  description,
}: {
  open: boolean;
  onOpenChange: (next: boolean) => void;
  value: IssueHandoffData | null | undefined;
  onSave: (next: IssueHandoffData | null) => void;
  /** Override the default dialog title. Defaults to the i18n
   *  `detail.handoff_data_dialog_title` key. */
  title?: string;
  /** Override the default dialog description. */
  description?: string;
}) {
  const { t } = useT("issues");
  const [draft, setDraft] = useState(() => serializeHandoffData(value));
  const [error, setError] = useState<ParseErrorReason | null>(null);

  // Re-seed the textarea when the dialog re-opens, so an external
  // mutation (e.g. issue-detail page refreshed the issue) doesn't get
  // clobbered by stale local draft state.
  useEffect(() => {
    if (open) {
      setDraft(serializeHandoffData(value));
      setError(null);
    }
  }, [open, value]);

  const charCount = draft.length;
  const overSize = charCount > HANDOFF_DATA_MAX_BYTES;

  const handleSave = () => {
    const result = parseHandoffData(draft);
    if (!result.ok) {
      setError(result.reason);
      return;
    }
    if (Object.keys(result.value).length === 0) {
      onSave(null);
    } else {
      onSave(result.value);
    }
    onOpenChange(false);
  };

  const handleClear = () => {
    setDraft("");
    setError(null);
    onSave(null);
    onOpenChange(false);
  };

  const placeholder = t(($) => $.detail.handoff_data_placeholder);

  const errorMessage = useMemo(() => {
    switch (error) {
      case "invalid_json":
        return t(($) => $.detail.handoff_data_invalid_json);
      case "not_object":
        return t(($) => $.detail.handoff_data_not_object);
      case "too_large":
        return t(($) => $.detail.handoff_data_too_large);
      default:
        return null;
    }
  }, [error, t]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title ?? t(($) => $.detail.handoff_data_dialog_title)}</DialogTitle>
          <DialogDescription>
            {description ?? t(($) => $.detail.handoff_data_dialog_description)}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-2">
          <div className="relative">
            <Textarea
              value={draft}
              onChange={(e) => {
                setDraft(e.target.value);
                if (error) setError(null);
              }}
              placeholder={placeholder}
              className={cn(
                "min-h-[260px] font-mono text-xs leading-relaxed",
                errorMessage && "border-destructive focus-visible:ring-destructive/30",
              )}
              spellCheck={false}
              autoComplete="off"
            />
          </div>
          <div className="flex items-center justify-between text-[11px] text-muted-foreground">
            <div className="flex items-center gap-1.5">
              <Braces className="size-3" />
              <span>JSON object</span>
            </div>
            <span className={cn("tabular-nums", overSize && "text-destructive")}>
              {charCount} / {HANDOFF_DATA_MAX_BYTES}
            </span>
          </div>
          {errorMessage && (
            <p className="text-[11px] text-destructive">{errorMessage}</p>
          )}
        </div>

        <DialogFooter className="flex items-center justify-between gap-2 sm:justify-between">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={handleClear}
            className="text-muted-foreground"
            disabled={draft.length === 0}
          >
            <XIcon className="size-3.5" />
            {t(($) => $.detail.handoff_data_empty)}
          </Button>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => onOpenChange(false)}
            >
              {t(($) => $.detail.handoff_data_cancel)}
            </Button>
            <Button type="button" size="sm" onClick={handleSave} disabled={overSize}>
              <Check className="size-3.5" />
              {t(($) => $.detail.handoff_data_save)}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
