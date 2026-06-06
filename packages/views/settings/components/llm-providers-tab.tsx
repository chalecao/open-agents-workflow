"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Pencil, Plus, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@multica/ui/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import {
  llmProvidersOptions,
  useCreateLLMProvider,
  useDeleteLLMProvider,
  useUpdateLLMProvider,
} from "@multica/core/llm-providers";
import type { LLMProvider } from "@multica/core/types";
import { useT } from "../../i18n";

// LLM Providers is the workspace settings panel for OpenAI-compatible
// LLM endpoints (cloud or local). Each provider is auto-paired with an
// `openai-http` runtime row that the agent picker surfaces; agents
// bound to that runtime execute against the configured endpoint via
// the server-side LLM execution worker.
//
// Listing is member-visible (the agent picker needs to read it);
// create / update / delete are admin-only. The UI mirrors the
// server's role check by hiding the destructive and write affordances
// for non-admins, but the server is the source of truth and would
// 403 anyway.

type EditingState =
  | { kind: "closed" }
  | { kind: "create" }
  | { kind: "edit"; provider: LLMProvider };

type FormState = {
  name: string;
  base_url: string;
  api_key: string;
  // `api_key_set` is the only signal from the server that a key is
  // already configured (the secretbox ciphertext is never returned).
  // On edit, the form starts with `api_key` empty and the user can
  // either leave it empty (= no change) or type a new value. The
  // "Replace API key" checkbox is a UI affordance to make the
  // round-trip-safe contract explicit: the empty `api_key` field
  // means "no change" by default, so the user has to opt in.
  api_key_set: boolean;
  api_key_replace: boolean;
  model_name: string;
};

const EMPTY_FORM: FormState = {
  name: "",
  base_url: "",
  api_key: "",
  api_key_set: false,
  api_key_replace: false,
  model_name: "",
};

const NAME_MAX = 64;
const MODEL_MAX = 200;

function formForProvider(p: LLMProvider | null): FormState {
  if (!p) return EMPTY_FORM;
  return {
    name: p.name,
    base_url: p.base_url,
    api_key: "",
    api_key_set: p.has_api_key,
    api_key_replace: false,
    model_name: p.model_name,
  };
}

function validateForm(form: FormState): { ok: true } | { ok: false; reason: string } {
  const name = form.name.trim();
  if (!name) return { ok: false, reason: "name_required" };
  if ([...name].length > NAME_MAX) return { ok: false, reason: "name_too_long" };
  const base = form.base_url.trim();
  if (!base) return { ok: false, reason: "base_url_required" };
  // Mirror the server's URL validation here so the user sees the
  // error inline before a wasted round-trip. We deliberately do not
  // ping the host — that would happen on the worker's first task
  // and the create flow shouldn't depend on the endpoint being up.
  let parsed: URL;
  try {
    parsed = new URL(base);
  } catch {
    return { ok: false, reason: "base_url_invalid" };
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    return { ok: false, reason: "base_url_protocol" };
  }
  if (!parsed.host) return { ok: false, reason: "base_url_no_host" };
  if (parsed.username || parsed.password) {
    return { ok: false, reason: "base_url_credentials" };
  }
  const model = form.model_name.trim();
  if (!model) return { ok: false, reason: "model_required" };
  if (model.length > MODEL_MAX) return { ok: false, reason: "model_too_long" };
  return { ok: true };
}

export function LLMProvidersTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data, isLoading } = useQuery({
    ...llmProvidersOptions(wsId),
    enabled: !!wsId,
  });
  const providers = data ?? [];

  const [editing, setEditing] = useState<EditingState>({ kind: "closed" });
  const [form, setForm] = useState<FormState>(EMPTY_FORM);
  const [deleteTarget, setDeleteTarget] = useState<LLMProvider | null>(null);

  const createMut = useCreateLLMProvider();
  const updateMut = useUpdateLLMProvider();
  const deleteMut = useDeleteLLMProvider();

  // When the dialog target changes, seed the form once. The mount-only
  // effect avoids re-seeding on every keystroke (re-renders during
  // typing would otherwise clobber the user's input).
  useEffect(() => {
    if (editing.kind === "edit") {
      setForm(formForProvider(editing.provider));
    } else if (editing.kind === "create") {
      setForm(EMPTY_FORM);
    }
  }, [editing]);

  const formError = (reason: string): string => {
    const map: Record<string, string> = {
      name_required: t(($) => $.llm_providers.error_name_required),
      name_too_long: t(($) => $.llm_providers.error_name_too_long, { max: NAME_MAX }),
      base_url_required: t(($) => $.llm_providers.error_base_url_required),
      base_url_invalid: t(($) => $.llm_providers.error_base_url_invalid),
      base_url_protocol: t(($) => $.llm_providers.error_base_url_protocol),
      base_url_no_host: t(($) => $.llm_providers.error_base_url_no_host),
      base_url_credentials: t(($) => $.llm_providers.error_base_url_credentials),
      model_required: t(($) => $.llm_providers.error_model_required),
      model_too_long: t(($) => $.llm_providers.error_model_too_long, { max: MODEL_MAX }),
    };
    return map[reason] ?? t(($) => $.llm_providers.error_generic);
  };

  async function handleSave() {
    const v = validateForm(form);
    if (!v.ok) {
      toast.error(formError(v.reason));
      return;
    }
    try {
      if (editing.kind === "create") {
        await createMut.mutateAsync({
          name: form.name.trim(),
          base_url: form.base_url.trim().replace(/\/+$/, ""),
          api_key: form.api_key.trim() || undefined,
          model_name: form.model_name.trim(),
        });
        toast.success(t(($) => $.llm_providers.toast_created));
      } else if (editing.kind === "edit") {
        const id = editing.provider.id;
        // Build the patch — the round-trip-safe api_key contract from
        // the server is replicated here. Empty `api_key` on edit means
        // "no change" UNLESS `api_key_replace` is on, in which case an
        // empty value means "clear the key" via clear_api_key.
        const patch: {
          name?: string;
          base_url?: string;
          model_name?: string;
          api_key?: string;
          clear_api_key?: boolean;
        } = {
          name: form.name.trim(),
          base_url: form.base_url.trim().replace(/\/+$/, ""),
          model_name: form.model_name.trim(),
        };
        if (form.api_key_replace) {
          const trimmed = form.api_key.trim();
          if (trimmed) {
            patch.api_key = trimmed;
          } else {
            patch.clear_api_key = true;
          }
        }
        await updateMut.mutateAsync({ id, ...patch });
        toast.success(t(($) => $.llm_providers.toast_updated));
      }
      setEditing({ kind: "closed" });
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.llm_providers.toast_save_failed),
      );
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    try {
      await deleteMut.mutateAsync(deleteTarget.id);
      toast.success(t(($) => $.llm_providers.toast_deleted));
      setDeleteTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.llm_providers.toast_delete_failed),
      );
    }
  }

  const isSaving = createMut.isPending || updateMut.isPending;

  return (
    <div className="space-y-8">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">
          {t(($) => $.llm_providers.page_description)}
        </p>
      </section>

      <section className="space-y-3">
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-sm font-semibold">
            {t(($) => $.llm_providers.section_configured)}
          </h2>
          {canManage && (
            <Button
              size="sm"
              onClick={() => setEditing({ kind: "create" })}
              data-testid="llm-providers-add"
            >
              <Plus className="h-3 w-3" />
              {t(($) => $.llm_providers.add)}
            </Button>
          )}
        </div>

        {!canManage && (
          <p className="text-xs text-muted-foreground">
            {t(($) => $.llm_providers.read_only_hint)}
          </p>
        )}

        {isLoading ? (
          <Card>
            <CardContent>
              <p className="text-sm text-muted-foreground">
                {t(($) => $.llm_providers.loading)}
              </p>
            </CardContent>
          </Card>
        ) : providers.length === 0 ? (
          <Card>
            <CardContent className="space-y-2">
              <p className="text-sm font-medium">
                {t(($) => $.llm_providers.empty_title)}
              </p>
              <p className="text-xs text-muted-foreground">
                {t(($) => $.llm_providers.empty_description)}
              </p>
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardContent className="divide-y">
              {providers.map((p) => (
                <ProviderRow
                  key={p.id}
                  provider={p}
                  canManage={canManage}
                  onEdit={() => setEditing({ kind: "edit", provider: p })}
                  onDelete={() => setDeleteTarget(p)}
                />
              ))}
            </CardContent>
          </Card>
        )}
      </section>

      {/* Add / edit dialog */}
      <Dialog
        open={editing.kind !== "closed"}
        onOpenChange={(open) => {
          if (!open && !isSaving) setEditing({ kind: "closed" });
        }}
      >
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>
              {editing.kind === "edit"
                ? t(($) => $.llm_providers.edit_title)
                : t(($) => $.llm_providers.add_title)}
            </DialogTitle>
            <DialogDescription>
              {t(($) => $.llm_providers.dialog_description)}
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="llm-provider-name">
                {t(($) => $.llm_providers.field_name)}
              </Label>
              <Input
                id="llm-provider-name"
                value={form.name}
                maxLength={NAME_MAX}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                placeholder={t(($) => $.llm_providers.field_name_placeholder)}
                data-testid="llm-provider-name"
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="llm-provider-base-url">
                {t(($) => $.llm_providers.field_base_url)}
              </Label>
              <Input
                id="llm-provider-base-url"
                value={form.base_url}
                onChange={(e) =>
                  setForm((f) => ({ ...f, base_url: e.target.value }))
                }
                placeholder={t(($) => $.llm_providers.field_base_url_placeholder)}
                data-testid="llm-provider-base-url"
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="llm-provider-api-key">
                {t(($) => $.llm_providers.field_api_key)}
              </Label>
              <Input
                id="llm-provider-api-key"
                type="password"
                autoComplete="off"
                value={form.api_key}
                onChange={(e) =>
                  setForm((f) => ({ ...f, api_key: e.target.value }))
                }
                placeholder={
                  editing.kind === "edit" && form.api_key_set
                    ? t(($) => $.llm_providers.field_api_key_placeholder_set)
                    : t(($) => $.llm_providers.field_api_key_placeholder_unset)
                }
                disabled={
                  editing.kind === "edit" && !form.api_key_replace
                }
                data-testid="llm-provider-api-key"
              />
              {editing.kind === "edit" && (
                <label className="flex items-center gap-2 pt-1 text-xs text-muted-foreground">
                  <input
                    type="checkbox"
                    checked={form.api_key_replace}
                    onChange={(e) =>
                      setForm((f) => ({
                        ...f,
                        api_key_replace: e.target.checked,
                        api_key: e.target.checked ? f.api_key : "",
                      }))
                    }
                    data-testid="llm-provider-api-key-replace"
                  />
                  {t(($) => $.llm_providers.api_key_replace_label)}
                </label>
              )}
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="llm-provider-model">
                {t(($) => $.llm_providers.field_model)}
              </Label>
              <Input
                id="llm-provider-model"
                value={form.model_name}
                maxLength={MODEL_MAX}
                onChange={(e) =>
                  setForm((f) => ({ ...f, model_name: e.target.value }))
                }
                placeholder={t(($) => $.llm_providers.field_model_placeholder)}
                data-testid="llm-provider-model"
              />
            </div>
          </div>

          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setEditing({ kind: "closed" })}
              disabled={isSaving}
            >
              {t(($) => $.llm_providers.cancel)}
            </Button>
            <Button
              size="sm"
              onClick={handleSave}
              disabled={isSaving}
              data-testid="llm-provider-save"
            >
              {isSaving
                ? t(($) => $.llm_providers.saving)
                : editing.kind === "edit"
                  ? t(($) => $.llm_providers.save_changes)
                  : t(($) => $.llm_providers.add_provider)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(v) => {
          if (!v && !deleteMut.isPending) setDeleteTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.llm_providers.delete_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.llm_providers.delete_confirm_description, {
                name: deleteTarget?.name ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteMut.isPending}>
              {t(($) => $.llm_providers.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDelete} disabled={deleteMut.isPending}>
              {deleteMut.isPending
                ? t(($) => $.llm_providers.deleting)
                : t(($) => $.llm_providers.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ProviderRow({
  provider,
  canManage,
  onEdit,
  onDelete,
}: {
  provider: LLMProvider;
  canManage: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useT("settings");
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="min-w-0 space-y-1">
        <p className="text-sm font-medium">
          {provider.name}
          {provider.has_api_key && (
            <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
              {t(($) => $.llm_providers.api_key_set_badge)}
            </span>
          )}
        </p>
        <p className="break-all font-mono text-[10px] text-muted-foreground">
          {provider.base_url}
        </p>
        <p className="text-[10px] text-muted-foreground">
          {t(($) => $.llm_providers.model_label, { model: provider.model_name })}
        </p>
      </div>
      {canManage && (
        <div className="flex shrink-0 items-center gap-1">
          <Button
            variant="outline"
            size="sm"
            onClick={onEdit}
            aria-label={t(($) => $.llm_providers.edit_aria, { name: provider.name })}
            data-testid="llm-provider-edit"
          >
            <Pencil className="h-3 w-3" />
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={onDelete}
            aria-label={t(($) => $.llm_providers.delete_aria, { name: provider.name })}
            data-testid="llm-provider-delete"
          >
            <Trash2 className="h-3 w-3" />
          </Button>
        </div>
      )}
    </div>
  );
}
