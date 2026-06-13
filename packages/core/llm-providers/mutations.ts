import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { llmProviderKeys } from "./queries";
import { useWorkspaceId } from "../hooks";
import { runtimeKeys } from "../runtimes/queries";
import type {
  LLMProvider,
  CreateLLMProviderRequest,
  UpdateLLMProviderRequest,
} from "../types";

// LLM provider mutations. Cache update strategy mirrors the labels
// mutations: optimistic add on create, snapshot+rollback on update /
// delete, and a full invalidate on settle so the runtime list — which
// also surfaces the auto-paired `openai-http` row — refetches in lock
// step. A provider delete cascades the runtime, so the runtime list
// also needs a refetch.

/** POST /api/llm-providers/. Admin-only on the server. On success,
 *  prepend the new provider to the list and invalidate the runtime
 *  list so the agent picker surfaces the new `openai-http` row. */
export function useCreateLLMProvider() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (data: CreateLLMProviderRequest) => api.createLLMProvider(data),
    onSuccess: (provider) => {
      qc.setQueryData<LLMProvider[]>(llmProviderKeys.list(wsId), (old) =>
        old && !old.some((p) => p.id === provider.id)
          ? [provider, ...old]
          : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: llmProviderKeys.all(wsId) });
      // The provider auto-pairs a runtime; the agent picker reads from
      // the runtime list, so it has to refetch too.
      qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
    },
  });
}

/** PATCH /api/llm-providers/{id}. Admin-only. Optimistic: apply the
 *  patch to the list cache, snapshot for rollback, invalidate on
 *  settle. The provider's `runtime_id` is stable across edits, so
 *  re-pinning the runtime picker is unnecessary on success. */
export function useUpdateLLMProvider() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({
      id,
      ...data
    }: { id: string } & UpdateLLMProviderRequest) =>
      api.updateLLMProvider(id, data),
    onMutate: async ({ id, ...data }) => {
      await qc.cancelQueries({ queryKey: llmProviderKeys.list(wsId) });
      const prevList = qc.getQueryData<LLMProvider[]>(
        llmProviderKeys.list(wsId),
      );
      qc.setQueryData<LLMProvider[]>(llmProviderKeys.list(wsId), (old) =>
        old
          ? old.map((p) =>
              p.id === id
                ? {
                    ...p,
                    ...(data.name !== undefined ? { name: data.name } : {}),
                    ...(data.base_url !== undefined
                      ? { base_url: data.base_url }
                      : {}),
                    ...(data.model_name !== undefined
                      ? { model_name: data.model_name }
                      : {}),
                    // api_key edits collapse to a "has_api_key" hint in
                    // the cache; we never store the secret in the
                    // client. clear_api_key forces false, supplying a
                    // new key preserves / sets true. The server's
                    // round-trip-safe contract means we can't tell a
                    // no-op "empty api_key" from a clear from the wire
                    // alone, so the optimistic value is best-effort.
                    ...(data.clear_api_key
                      ? { has_api_key: false }
                      : data.api_key !== undefined &&
                          data.api_key.length > 0
                        ? { has_api_key: true }
                        : {}),
                  }
                : p,
            )
          : old,
      );
      return { prevList, id };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.prevList) {
        qc.setQueryData(llmProviderKeys.list(wsId), ctx.prevList);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: llmProviderKeys.all(wsId) });
    },
  });
}

/** DELETE /api/llm-providers/{id}. Admin-only. The server cascades
 *  the delete to the auto-paired runtime row, so the runtime list
 *  must refetch too — otherwise the agent picker keeps offering a
 *  runtime whose underlying provider is gone. */
export function useDeleteLLMProvider() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (id: string) => api.deleteLLMProvider(id),
    onMutate: async (id) => {
      await qc.cancelQueries({ queryKey: llmProviderKeys.list(wsId) });
      const prevList = qc.getQueryData<LLMProvider[]>(
        llmProviderKeys.list(wsId),
      );
      qc.setQueryData<LLMProvider[]>(llmProviderKeys.list(wsId), (old) =>
        old ? old.filter((p) => p.id !== id) : old,
      );
      return { prevList };
    },
    onError: (_err, _id, ctx) => {
      if (ctx?.prevList) {
        qc.setQueryData(llmProviderKeys.list(wsId), ctx.prevList);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: llmProviderKeys.all(wsId) });
      // Cascade: the paired runtime is gone too. Refetch the runtime
      // list so the agent picker drops the orphan runtime.
      qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
    },
  });
}
