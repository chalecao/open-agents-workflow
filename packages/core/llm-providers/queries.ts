import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for LLM provider reads. Realtime sync (and the
 *  mutation hooks in ./mutations) invalidate the list on
 *  `llm_provider:*` events so the Settings panel updates without a
 *  refetch, mirroring the larkKeys pattern. */
export const llmProviderKeys = {
  all: (wsId: string) => ["llm-providers", wsId] as const,
  list: (wsId: string) => [...llmProviderKeys.all(wsId), "list"] as const,
  detail: (wsId: string, id: string) =>
    [...llmProviderKeys.all(wsId), "detail", id] as const,
};

/** Workspace-scoped LLM provider list. Reads are member-visible; the
 *  agent picker surfaces the auto-paired `openai-http` runtimes, so
 *  member role is enough to render the picker chip. */
export const llmProvidersOptions = (wsId: string) =>
  queryOptions({
    queryKey: llmProviderKeys.list(wsId),
    queryFn: () => api.listLLMProviders(),
    enabled: !!wsId,
  });

/** Single-provider detail. Used by the edit form prefetch; the list
 *  is the source of truth for the listing card. */
export const llmProviderOptions = (wsId: string, providerId: string) =>
  queryOptions({
    queryKey: llmProviderKeys.detail(wsId, providerId),
    queryFn: () => api.getLLMProvider(providerId),
    enabled: !!wsId && !!providerId,
  });
