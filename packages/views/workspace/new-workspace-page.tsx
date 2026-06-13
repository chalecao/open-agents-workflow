"use client";

import { ArrowLeft, LogOut } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import type { Workspace } from "@multica/core/types";
import { useConfigStore } from "@multica/core/config";
import { useIsPlatformAdmin } from "@multica/core/auth";
import { useLogout } from "../auth";
import { DragStrip } from "../platform";
import { useT } from "../i18n";
import { CreateWorkspaceForm } from "./create-workspace-form";

/**
 * Full-page shell for the "create workspace" transition. Shared between web
 * (Next.js route `/workspaces/new`) and desktop (window-overlay). The
 * top-bar affordances — Back (when dismissable) and Log out — live here
 * so both platforms get identical UX; platform-specific concerns like
 * window-drag region and macOS traffic-light handling stay in each app's
 * shell.
 *
 * `onBack` is optional: caller passes it only when there's somewhere to go
 * back to (user has other workspaces, or the flow was entered from an
 * existing session). On the zero-workspace entry path it's omitted, which
 * hides Back — Log out is then the only escape.
 *
 * The platform-admin gate is layered on top of the existing
 * `workspaceCreationDisabled` flag: even on deployments that have not set
 * `DISABLE_WORKSPACE_CREATION=true`, non-admins (users not on the
 * operator-configured `MULTICA_ADMIN_EMAILS` allowlist) are shown the
 * "you need an invitation" empty state instead of the create form. The
 * 403 on the server is the authoritative check; this is the UI mirror.
 *
 * We combine two signals here: `adminEmailsConfigured` (the env var is
 * non-empty) AND `!isPlatformAdmin` (the caller is not on the allowlist).
 * Both must be true to lock the form — if MULTICA_ADMIN_EMAILS is unset
 * the gate is off and the previous "any user can create" behavior
 * applies regardless of the per-user `is_platform_admin` flag.
 */
export function NewWorkspacePage({
  onSuccess,
  onBack,
}: {
  onSuccess: (workspace: Workspace) => void;
  onBack?: () => void;
}) {
  const { t } = useT("workspace");
  const logout = useLogout();
  const workspaceCreationDisabled = useConfigStore((s) => s.workspaceCreationDisabled);
  const adminEmailsConfigured = useConfigStore((s) => s.adminEmailsConfigured);
  const isPlatformAdmin = useIsPlatformAdmin();
  const showAdminLockedState = adminEmailsConfigured && !isPlatformAdmin;

  return (
    <div className="relative flex min-h-svh flex-col bg-background">
      <DragStrip />
      {onBack && (
        <Button
          variant="ghost"
          size="sm"
          className="absolute top-16 left-12 text-muted-foreground"
          onClick={onBack}
        >
          <ArrowLeft />
          {t(($) => $.new_page.back)}
        </Button>
      )}
      <Button
        variant="ghost"
        size="sm"
        className="absolute top-16 right-12 text-muted-foreground hover:text-destructive"
        onClick={logout}
      >
        <LogOut />
        {t(($) => $.new_page.log_out)}
      </Button>

      <div className="flex flex-1 flex-col items-center justify-center px-6 pb-12">
        <div className="flex w-full max-w-md flex-col items-center gap-6">
          {workspaceCreationDisabled ? (
            <div className="text-center">
              <h1 className="text-3xl font-semibold tracking-tight">
                {t(($) => $.creation_disabled.title)}
              </h1>
              <p className="mt-3 text-muted-foreground">
                {t(($) => $.creation_disabled.description)}
              </p>
            </div>
          ) : showAdminLockedState ? (
            <div className="text-center">
              <h1 className="text-3xl font-semibold tracking-tight">
                {t(($) => $.admin_locked.title)}
              </h1>
              <p className="mt-3 text-muted-foreground">
                {t(($) => $.admin_locked.description)}
              </p>
            </div>
          ) : (
            <>
              <div className="text-center">
                <h1 className="text-3xl font-semibold tracking-tight">
                  {t(($) => $.new_page.title)}
                </h1>
                <p className="mt-3 text-muted-foreground">
                  {t(($) => $.new_page.description)}
                </p>
              </div>
              <CreateWorkspaceForm onSuccess={onSuccess} />
              <p className="text-center text-xs text-muted-foreground">
                {t(($) => $.new_page.invite_hint)}
              </p>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
