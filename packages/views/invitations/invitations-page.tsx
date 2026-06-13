"use client";

import { useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useAuthStore, useIsPlatformAdmin } from "@multica/core/auth";
import { useConfigStore } from "@multica/core/config";
import {
  myInvitationListOptions,
  workspaceKeys,
  workspaceListOptions,
} from "@multica/core/workspace/queries";
import { paths } from "@multica/core/paths";
import type { Invitation } from "@multica/core/types";
import { useNavigation } from "../navigation";
import { useLogout } from "../auth";
import { DragStrip } from "../platform";
import { useT } from "../i18n";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { LogOut, Mail, Users } from "lucide-react";

/**
 * Batch invitation handling page for first-contact users who land here
 * because callback / login detected pending invitations on their email.
 *
 * Design:
 *  - This route is only reachable for un-onboarded users (the entry-point
 *    judgment in callback/login routes already-onboarded users straight
 *    into their workspace; new invites for those users surface in the
 *    sidebar's pending-invitations dropdown instead).
 *  - The user picks zero or more invitations to accept. "Submit" then:
 *      • zero selected → continue to /onboarding
 *      • ≥1 selected → accept each, mark onboarding complete, navigate
 *        into the first accepted workspace.
 *  - Unselected invitations are intentionally left as `pending` in the DB.
 *    The user can later decline them from the sidebar; we don't auto-decline
 *    here because closing/refreshing this page should not be a destructive
 *    action.
 */
export function InvitationsPage() {
  const { t } = useT("invite");
  const { push } = useNavigation();
  const qc = useQueryClient();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Platform-admin gate: when the operator has configured
  // MULTICA_ADMIN_EMAILS and the caller is not on the allowlist, the
  // user can ONLY join a workspace by accepting an invitation. We hide
  // the "set up my own workspace" affordance and reword the
  // empty-state CTA so the page never claims the user can create
  // something they actually cannot.
  const adminEmailsConfigured = useConfigStore((s) => s.adminEmailsConfigured);
  const isPlatformAdmin = useIsPlatformAdmin();
  const platformAdminLocked = adminEmailsConfigured && !isPlatformAdmin;

  const {
    data: invitations,
    isLoading,
    error: fetchError,
    refetch,
  } = useQuery(myInvitationListOptions());

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const handleSubmit = async () => {
    setError(null);

    // Zero selected: hand off to onboarding. Pending invites stay pending and
    // can be picked up later from the sidebar.
    if (selected.size === 0) {
      push(paths.onboarding());
      return;
    }

    setSubmitting(true);
    const acceptedIds: string[] = [];
    try {
      for (const id of selected) {
        await api.acceptInvitation(id);
        acceptedIds.push(id);
      }

      const firstAcceptedInvite = invitations?.find(
        (inv) => inv.id === acceptedIds[0],
      );

      // markOnboardingComplete is a frontend-side belt to the backend braces:
      // each AcceptInvitation transaction already sets onboarded_at via
      // MarkUserOnboarded, but calling this from the client makes sure the
      // returned `User` is freshly written and gives refreshMe something
      // canonical to read.
      await api.markOnboardingComplete({
        completion_path: "invite_accept",
        workspace_id: firstAcceptedInvite?.workspace_id,
      });
      await useAuthStore.getState().refreshMe();

      qc.invalidateQueries({ queryKey: workspaceKeys.myInvitations() });
      const wsList = await qc.fetchQuery({
        ...workspaceListOptions(),
        staleTime: 0,
      });

      const targetWs = firstAcceptedInvite
        ? wsList.find((w) => w.id === firstAcceptedInvite.workspace_id)
        : undefined;

      // If we can't resolve the just-accepted workspace by id (shouldn't
      // happen — the backend just inserted the membership and we just
      // refetched), fall back to the resolver. Don't blindly route to
      // wsList[0]: that could teleport the user into an unrelated old
      // workspace they happen to also belong to.
      push(
        targetWs ? paths.workspace(targetWs.slug).issues() : paths.newWorkspace(),
      );
    } catch (e) {
      setError(
        e instanceof Error
          ? e.message
          : t(($) => $.batch.error_generic),
      );
      // Partial success: any accepts that landed before the failure ALREADY
      // set onboarded_at on the backend (the AcceptInvitation transaction
      // is atomic per invite). Refresh local user + workspace state so the
      // sidebar reflects the partial accept and the user isn't stuck with a
      // stale `onboarded_at == null` view. The next submit is safe — the
      // server returns 4xx on re-accept and the catch path will surface that.
      if (acceptedIds.length > 0) {
        await useAuthStore.getState().refreshMe().catch(() => {});
        qc.invalidateQueries({ queryKey: workspaceKeys.list() });
      }
      qc.invalidateQueries({ queryKey: workspaceKeys.myInvitations() });
      refetch();
    } finally {
      setSubmitting(false);
    }
  };

  if (isLoading) {
    return (
      <InvitationsShell>
        <Card className="w-full max-w-lg">
          <CardContent className="flex flex-col gap-4 py-12">
            <Skeleton className="h-6 w-48" />
            <Skeleton className="h-4 w-72" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </CardContent>
        </Card>
      </InvitationsShell>
    );
  }

  // Empty / error: send the user on to onboarding so they're never stuck.
  // Genuine fetch failure is rare; treating it as "no invites" is safer than
  // trapping the user on an error screen they can't act on.
  //
  // For platform-admin-locked users we instead show a "no pending
  // invitations, ask your operator" empty state with no "Continue"
  // button — they cannot create their own workspace, so there is
  // nothing for them to continue to.
  if (fetchError || !invitations || invitations.length === 0) {
    return (
      <InvitationsShell>
        <Card className="w-full max-w-md">
          <CardContent className="flex flex-col items-center gap-4 py-12">
            <div className="flex h-12 w-12 items-center justify-center rounded-full bg-muted">
              <Mail className="h-6 w-6 text-muted-foreground" />
            </div>
            <h2 className="text-lg font-semibold">{t(($) => $.batch.empty_title)}</h2>
            <p className="text-sm text-muted-foreground text-center">
              {platformAdminLocked
                ? t(($) => $.batch.empty_hint_locked)
                : t(($) => $.batch.empty_hint)}
            </p>
            {!platformAdminLocked && (
              <Button onClick={() => push(paths.onboarding())}>
                {t(($) => $.batch.empty_continue)}
              </Button>
            )}
          </CardContent>
        </Card>
      </InvitationsShell>
    );
  }

  const submitLabel = (() => {
    if (selected.size > 0) {
      // i18next collapses `submit_join_one` / `submit_join_other` into
      // a single `submit_join` plural value. Pass the count so it picks
      // the right form.
      return t(($) => $.batch.submit_join, { count: selected.size });
    }
    if (platformAdminLocked) {
      // Non-admin with the platform-admin gate active: the only way
      // to join a workspace is by accepting an invitation. Reword
      // the skip button so it doesn't claim the user can create
      // their own.
      return t(($) => $.batch.submit_skip_locked);
    }
    return t(($) => $.batch.submit_skip);
  })();

  return (
    <InvitationsShell>
      <Card className="w-full max-w-lg">
        <CardContent className="flex flex-col gap-6 py-10">
          <div className="flex flex-col items-center gap-3 text-center">
            <div className="flex h-12 w-12 items-center justify-center rounded-full bg-primary/10">
              <Users className="h-6 w-6 text-primary" />
            </div>
            <div className="space-y-1">
              <h2 className="text-xl font-semibold">
                {t(($) => $.batch.title)}
              </h2>
              <p className="text-sm text-muted-foreground">
                {t(($) => $.batch.subtitle)}
              </p>
            </div>
          </div>

          <ul className="flex flex-col gap-2">
            {invitations.map((inv) => (
              <InvitationRow
                key={inv.id}
                invitation={inv}
                checked={selected.has(inv.id)}
                onToggle={() => toggle(inv.id)}
              />
            ))}
          </ul>

          <Button
            className="w-full"
            onClick={handleSubmit}
            disabled={submitting}
          >
            {submitting ? t(($) => $.batch.joining) : submitLabel}
          </Button>

          {error && (
            <p className="text-sm text-destructive text-center">{error}</p>
          )}
        </CardContent>
      </Card>
    </InvitationsShell>
  );
}

function InvitationRow({
  invitation,
  checked,
  onToggle,
}: {
  invitation: Invitation;
  checked: boolean;
  onToggle: () => void;
}) {
  const { t } = useT("invite");
  const inviter =
    invitation.inviter_name ||
    invitation.inviter_email ||
    t(($) => $.batch.row_inviter_fallback);
  const roleLine =
    invitation.role === "admin"
      ? t(($) => $.batch.row_invited_admin, { inviter })
      : t(($) => $.batch.row_invited_member, { inviter });
  return (
    <li>
      <label
        className="flex cursor-pointer items-start gap-3 rounded-md border border-border bg-card p-4 hover:bg-accent/40"
      >
        <Checkbox
          checked={checked}
          onCheckedChange={onToggle}
          className="mt-1"
        />
        <div className="flex-1 min-w-0 space-y-1">
          <div className="font-medium truncate">
            {invitation.workspace_name ?? t(($) => $.batch.row_workspace_fallback)}
          </div>
          <div className="text-xs text-muted-foreground truncate">
            {roleLine}
          </div>
        </div>
      </label>
    </li>
  );
}

function InvitationsShell({ children }: { children: ReactNode }) {
  const { t } = useT("invite");
  const logout = useLogout();
  return (
    <div className="relative flex min-h-svh flex-col bg-background">
      <DragStrip />
      <Button
        variant="ghost"
        size="sm"
        className="absolute top-16 right-12 text-muted-foreground hover:text-destructive"
        onClick={logout}
      >
        <LogOut />
        {t(($) => $.batch.log_out)}
      </Button>
      <div className="flex flex-1 flex-col items-center justify-center px-6 pb-12">
        {children}
      </div>
    </div>
  );
}
