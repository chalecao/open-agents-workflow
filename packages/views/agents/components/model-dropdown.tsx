"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Brain, ChevronDown, Cpu, Loader2, Plus, Check, Info } from "lucide-react";
import { runtimeModelsOptions } from "@multica/core/runtimes";
import type { RuntimeModel } from "@multica/core/types";
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
} from "@multica/ui/components/ui/popover";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useT } from "../../i18n";

// ModelDropdown renders a searchable, creatable model picker for an agent.
// It fetches the supported-model catalog from the selected runtime — the
// daemon enumerates models on demand via heartbeat piggyback. Providers
// that don't honour per-agent model selection at runtime (currently
// antigravity — `agy` has no `--model` flag and reads selection from
// its own settings, and qodercli — same situation) return
// supported=false, and the dropdown renders disabled with an explanation
// instead of silently accepting a value the backend would ignore.
//
// When the selected runtime is a workspace-configured LLM provider
// (provider="openai-http"), the model is fixed at the provider level
// (server/internal/llmexec reads `llm_provider.model_name` directly —
// the agent's `model` column is never consulted by the LLM worker). The
// caller passes the configured model via `llmModel` and the dropdown
// renders a read-only chip that mirrors the picker's chrome but exposes
// no editing surface, so the user can't accidentally type a value that
// would be ignored.
export function ModelDropdown({
  runtimeId,
  runtimeOnline,
  value,
  onChange,
  disabled,
  llmModel,
}: {
  runtimeId: string | null;
  runtimeOnline: boolean;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  /** When set, the runtime is backed by a workspace LLM provider and the
   *  model is fixed at the provider level. The dropdown renders the
   *  configured model as a read-only chip; the caller should pre-fill
   *  `value` with this string and not expose editing. */
  llmModel?: string | null;
}) {
  const { t } = useT("agents");
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");

  const isLLMRuntime = !!llmModel;
  const modelsQuery = useQuery(
    // LLM-provider runtimes have no daemon to ask for a model catalog —
    // the worker reads the configured model directly from the provider
    // row. Skip the heartbeat round-trip entirely so the dropdown never
    // paints an indeterminate "discovering" state for a runtime that
    // has no discovery surface.
    runtimeModelsOptions(isLLMRuntime || !runtimeOnline ? null : runtimeId),
  );

  const supported = modelsQuery.data?.supported ?? true;
  // Stable reference for the model list — `?? []` would mint a fresh
  // array each render and force every downstream useMemo to invalidate.
  const models = useMemo(
    () => modelsQuery.data?.models ?? [],
    [modelsQuery.data],
  );
  const grouped = useMemo(() => groupByProvider(models), [models]);

  // When the selected runtime reports it doesn't support per-agent
  // model selection, clear any previously-saved value so we don't
  // persist a ghost configuration that never takes effect.
  useEffect(() => {
    if (!supported && value !== "") {
      onChange("");
    }
  }, [supported, value, onChange]);

  const filtered = useMemo(() => {
    if (!search.trim()) return grouped;
    const needle = search.toLowerCase();
    const out: Record<string, RuntimeModel[]> = {};
    for (const [provider, list] of Object.entries(grouped)) {
      const matches = list.filter(
        (m) =>
          m.id.toLowerCase().includes(needle) ||
          m.label.toLowerCase().includes(needle),
      );
      if (matches.length > 0) out[provider] = matches;
    }
    return out;
  }, [grouped, search]);

  const trimmedSearch = search.trim();
  const exactMatch = models.some(
    (m) => m.id === trimmedSearch || m.label === trimmedSearch,
  );
  const canCreate = trimmedSearch.length > 0 && !exactMatch;

  const select = (id: string) => {
    onChange(id);
    setOpen(false);
    setSearch("");
  };

  const triggerLabel =
    value ||
    (disabled
      ? t(($) => $.model_dropdown.select_runtime_first)
      : runtimeOnline
        ? t(($) => $.model_dropdown.default_provider)
        : t(($) => $.model_dropdown.runtime_offline_manual));

  if (isLLMRuntime) {
    // LLM-provider runtime: model is fixed at the provider level. Show
    // a read-only chip that mirrors the picker's visual language (label
    // row, bordered container, model name) so the field reads as "the
    // model for this agent is X" without inviting an edit. The agent's
    // `model` column is populated by the parent dialog for downstream
    // display, but the LLM worker never reads it.
    return (
      <div className="flex flex-col min-w-0">
        <div className="flex h-6 items-center">
          <Label className="text-xs text-muted-foreground">{t(($) => $.model_dropdown.label)}</Label>
        </div>
        <div className="mt-1.5 flex items-center gap-3 rounded-lg border border-border bg-muted/30 px-3 py-2.5 text-sm">
          <Brain className="h-4 w-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <span className="truncate font-medium">{llmModel}</span>
            </div>
            <div className="truncate text-xs text-muted-foreground">
              {t(($) => $.model_dropdown.llm_fixed_hint)}
            </div>
          </div>
        </div>
      </div>
    );
  }

  if (!supported && !modelsQuery.isLoading) {
    return (
      <div className="flex flex-col min-w-0">
        <div className="flex h-6 items-center">
          <Label className="text-xs text-muted-foreground">{t(($) => $.model_dropdown.label)}</Label>
        </div>
        <div className="mt-1.5 flex items-start gap-2 rounded-lg border border-dashed border-border bg-muted/30 px-3 py-2.5 text-sm text-muted-foreground">
          <Info className="mt-0.5 h-4 w-4 shrink-0" />
          <div className="min-w-0">
            <div>{t(($) => $.model_dropdown.managed_by_runtime_title)}</div>
            <div className="mt-0.5 text-xs">
              {t(($) => $.model_dropdown.managed_by_runtime_hint)}
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col min-w-0">
      <div className="flex h-6 items-center justify-between">
        <Label className="text-xs text-muted-foreground">{t(($) => $.model_dropdown.label)}</Label>
        {modelsQuery.isError && (
          <span className="text-xs text-muted-foreground">{t(($) => $.model_dropdown.discovery_failed)}</span>
        )}
      </div>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          disabled={disabled}
          className="flex w-full min-w-0 items-center gap-3 rounded-lg border border-border bg-background px-3 py-2.5 mt-1.5 text-left text-sm transition-colors hover:bg-muted disabled:pointer-events-none disabled:opacity-50"
        >
          <Cpu className="h-4 w-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1">
            {/* Wrapped in flex to mirror RuntimePicker's trigger DOM. The
                two pickers sit side-by-side; inline-in-flex vs block-line-
                box height calc would otherwise leave them ~1px misaligned. */}
            <div className="flex items-center gap-2">
              <span className="truncate font-medium">{triggerLabel}</span>
            </div>
            {value && (
              <div className="truncate text-xs text-muted-foreground">
                {modelLabel(models, value)}
              </div>
            )}
          </div>
          <ChevronDown
            className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
          />
        </PopoverTrigger>
        <PopoverContent
          align="start"
          className="w-[var(--anchor-width)] p-0 overflow-hidden"
        >
          <div className="border-b border-border p-2">
            <Input
              autoFocus
              placeholder={t(($) => $.pickers.model_search_placeholder)}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="h-8"
            />
          </div>
          <div className="max-h-72 overflow-y-auto p-1">
            {modelsQuery.isLoading && (
              <div className="flex items-center gap-2 px-3 py-6 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t(($) => $.pickers.model_discovering)}
              </div>
            )}

            {!modelsQuery.isLoading &&
              Object.entries(filtered).map(([provider, list]) => (
                <div key={provider} className="mb-1">
                  {provider && (
                    <div className="px-2 pt-1.5 pb-0.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                      {provider}
                    </div>
                  )}
                  {list.map((m) => (
                    <button
                      type="button"
                      key={m.id}
                      onClick={() => select(m.id)}
                      className={`flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm transition-colors ${
                        m.id === value ? "bg-accent" : "hover:bg-accent/50"
                      }`}
                    >
                      <div className="min-w-0 flex-1">
                        <div className="truncate font-medium">{m.label}</div>
                        {m.label !== m.id && (
                          <div className="truncate text-xs text-muted-foreground">
                            {m.id}
                          </div>
                        )}
                      </div>
                      {m.id === value && (
                        <Check className="h-4 w-4 shrink-0 text-primary" />
                      )}
                    </button>
                  ))}
                </div>
              ))}

            {!modelsQuery.isLoading &&
              Object.keys(filtered).length === 0 &&
              !canCreate && (
                <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                  {t(($) => $.pickers.model_empty_with_dot)}
                </div>
              )}

            {canCreate && (
              <button
                type="button"
                onClick={() => select(trimmedSearch)}
                className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-primary transition-colors hover:bg-accent/50"
              >
                <Plus className="h-4 w-4 shrink-0" />
                <span className="truncate">
                  {t(($) => $.pickers.model_custom_use, { value: trimmedSearch })}
                </span>
              </button>
            )}

            {value && (
              <button
                type="button"
                onClick={() => select("")}
                className="mt-1 flex w-full items-center gap-2 border-t border-border px-3 py-2 text-left text-xs text-muted-foreground transition-colors hover:bg-accent/50"
              >
                {t(($) => $.model_dropdown.clear_full)}
              </button>
            )}
          </div>
        </PopoverContent>
      </Popover>
    </div>
  );
}

function groupByProvider(models: RuntimeModel[]): Record<string, RuntimeModel[]> {
  const out: Record<string, RuntimeModel[]> = {};
  for (const m of models) {
    const key = m.provider ?? "";
    if (!out[key]) out[key] = [];
    out[key].push(m);
  }
  return out;
}

function modelLabel(models: RuntimeModel[], id: string): string {
  const found = models.find((m) => m.id === id);
  if (!found) return "custom";
  return found.provider ? found.provider : "model";
}
