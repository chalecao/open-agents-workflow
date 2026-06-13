export { createAuthStore } from "./store";
export type { AuthStoreOptions, AuthState } from "./store";
export { sanitizeNextUrl } from "./utils";

import type { createAuthStore as CreateAuthStoreFn } from "./store";

type AuthStoreInstance = ReturnType<typeof CreateAuthStoreFn>;

/** Module-level singleton — set once at app boot via `registerAuthStore()`. */
let _store: AuthStoreInstance | null = null;

/**
 * Register the auth store instance created by the app.
 * Must be called at boot before any component renders.
 */
export function registerAuthStore(store: AuthStoreInstance) {
  _store = store;
}

/**
 * Singleton accessor — a Zustand hook backed by the registered instance.
 * Supports `useAuthStore(selector)` and `useAuthStore.getState()`.
 */
export const useAuthStore: AuthStoreInstance = new Proxy(
  (() => {}) as unknown as AuthStoreInstance,
  {
    apply(_target, _thisArg, args) {
      if (!_store)
        throw new Error(
          "Auth store not initialised — call registerAuthStore() first",
        );
      return (_store as unknown as (...a: unknown[]) => unknown)(...args);
    },
    get(_target, prop) {
      // Allow property inspection (HMR/React Refresh) before registration
      if (!_store) return undefined;
      return Reflect.get(_store, prop);
    },
  },
);

/**
 * `useIsPlatformAdmin` is a UI-only convenience selector over the
 * currently authenticated user. It returns true when the user is on
 * the operator-configured MULTICA_ADMIN_EMAILS allowlist (mirrored
 * by the backend as `is_platform_admin` on /api/me). The UI uses
 * it to hide "Create workspace" / "Settings" affordances for
 * non-admins; the server is the authoritative gate, so a forged
 * client value is harmless — the request will still 403.
 *
 * Returns false when the user is not loaded yet (cold boot, login
 * transition) so we never flash admin-only UI to a non-admin.
 * Also returns false when the auth store has not been registered
 * yet — this is a test-time / SSR safety net, not a real flow.
 */
export function useIsPlatformAdmin(): boolean {
  if (!_store) return false;
  return useAuthStore((s) => s.user?.is_platform_admin ?? false);
}
