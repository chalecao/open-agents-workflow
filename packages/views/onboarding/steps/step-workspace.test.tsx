import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enOnboarding from "../../locales/en/onboarding.json";
import enWorkspace from "../../locales/en/workspace.json";
import type { Workspace } from "@multica/core/types";

const TEST_RESOURCES = {
  en: {
    common: enCommon,
    onboarding: enOnboarding,
    workspace: enWorkspace,
  },
};

const mockLogout = vi.hoisted(() => vi.fn());
const mockUseConfigStore = vi.hoisted(() =>
  vi.fn(
    (selector: (state: {
      workspaceCreationDisabled: boolean;
      adminEmailsConfigured: boolean;
    }) => unknown) =>
      selector({ workspaceCreationDisabled: false, adminEmailsConfigured: false }),
  ),
);
const mockIsPlatformAdmin = vi.hoisted(() => vi.fn(() => false));

vi.mock("../../auth", () => ({
  useLogout: () => mockLogout,
}));

vi.mock("@multica/core/config", () => ({
  useConfigStore: (selector: (state: {
    workspaceCreationDisabled: boolean;
    adminEmailsConfigured: boolean;
  }) => unknown) => mockUseConfigStore(selector),
}));

vi.mock("@multica/core/auth", () => ({
  useIsPlatformAdmin: () => mockIsPlatformAdmin(),
}));

vi.mock("@multica/core/workspace/mutations", () => ({
  useCreateWorkspace: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("@multica/core/api", () => ({
  api: { getBaseUrl: () => "http://127.0.0.1:8080" },
}));

import { StepWorkspace } from "./step-workspace";

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function renderStep({
  existing,
  disabled,
  adminEmailsConfigured = false,
  isPlatformAdmin = false,
}: {
  existing: Workspace | null;
  disabled: boolean;
  adminEmailsConfigured?: boolean;
  isPlatformAdmin?: boolean;
}) {
  mockUseConfigStore.mockImplementation(
    (selector: (state: {
      workspaceCreationDisabled: boolean;
      adminEmailsConfigured: boolean;
    }) => unknown) =>
      selector({ workspaceCreationDisabled: disabled, adminEmailsConfigured }),
  );
  mockIsPlatformAdmin.mockReturnValue(isPlatformAdmin);
  return render(
    <StepWorkspace existing={existing} onCreated={vi.fn()} onBack={vi.fn()} />,
    { wrapper: I18nWrapper },
  );
}

const EXISTING_WORKSPACE: Workspace = {
  id: "00000000-0000-0000-0000-000000000001",
  name: "Acme",
  slug: "acme",
  description: null,
  context: null,
  settings: {},
  repos: [],
  issue_prefix: "ACM",
  created_at: "2025-01-01T00:00:00Z",
  updated_at: "2025-01-01T00:00:00Z",
} as unknown as Workspace;

// Regression for #3433 (PR feedback): when DISABLE_WORKSPACE_CREATION is on,
// every onboarding entry point must steer the user toward an existing
// workspace or a logout escape — never toward the create form, even
// indirectly (stale CTA copy, "or start another" prose, etc.).
describe("StepWorkspace — DISABLE_WORKSPACE_CREATION gate", () => {
  it("renders the create form when the flag is off and the user has no workspace", () => {
    renderStep({ existing: null, disabled: false });

    expect(
      screen.getByText("Name your workspace.", { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Workspace name")).toBeInTheDocument();
    expect(screen.getByLabelText("URL")).toBeInTheDocument();
  });

  it("hides the create form and shows the disabled notice when the flag is on and there is no workspace", () => {
    renderStep({ existing: null, disabled: true });

    expect(
      screen.getByText("Ask your administrator for an invitation.", {
        exact: false,
      }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Workspace name")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("URL")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /log out/i })).toBeInTheDocument();
  });

  it("forces the existing-workspace-only state when the flag is on and the user already has a workspace", () => {
    renderStep({ existing: EXISTING_WORKSPACE, disabled: true });

    // Disabled-specific copy is used in place of the "or start another" prose.
    expect(
      screen.getByText("Continue with Acme.", { exact: false }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/start another/i),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/create a new one alongside it/i),
    ).not.toBeInTheDocument();

    // Resume picker still shows the existing workspace card (its name
    // appears multiple times across avatar / card / side panel — at least
    // one is enough to know the card is rendered), but the "Create a new
    // workspace" radio card is gone entirely.
    expect(screen.getAllByText("Acme").length).toBeGreaterThan(0);
    expect(
      screen.queryByText("Create a new workspace", { exact: false }),
    ).not.toBeInTheDocument();

    // CTA is pre-selected to the existing-only action and immediately
    // enabled, so the user can press it without further interaction.
    const cta = screen.getByRole("button", { name: "Open Acme" });
    expect(cta).toBeEnabled();
  });
});

// Parallel coverage for the MULTICA_ADMIN_EMAILS gate. The DISABLE_WORKSPACE_CREATION
// tests above exercise the "operator disabled creation entirely" case; here we exercise
// the more common "only listed operators can create, others must wait for an invitation"
// case. Both lock the create path, but the copy is distinct because the user's next
// action is different: env-disabled usually means "instance is fully locked down" while
// platform-admin-locked means "you specifically need an operator to invite you".
describe("StepWorkspace — MULTICA_ADMIN_EMAILS gate", () => {
  it("shows operator-invitation copy when admin emails are configured and the user is not on the allowlist", () => {
    renderStep({
      existing: null,
      disabled: false,
      adminEmailsConfigured: true,
      isPlatformAdmin: false,
    });

    expect(
      screen.getByText("Wait for an invitation from an operator.", {
        exact: false,
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByLabelText("Workspace name"),
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("URL")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /log out/i })).toBeInTheDocument();
  });

  it("falls through to the normal create form when the allowlist is configured but the user IS an admin", () => {
    renderStep({
      existing: null,
      disabled: false,
      adminEmailsConfigured: true,
      isPlatformAdmin: true,
    });

    expect(
      screen.getByText("Name your workspace.", { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Workspace name")).toBeInTheDocument();
  });

  it("hides the create card but lets an invited non-admin resume into their existing workspace", () => {
    renderStep({
      existing: EXISTING_WORKSPACE,
      disabled: false,
      adminEmailsConfigured: true,
      isPlatformAdmin: false,
    });

    // Platform-admin-locked resume path uses its own "operator already
    // invited you" copy instead of the env-disabled "creation is
    // disabled" copy — the user's mental model is different.
    expect(
      screen.getByText("Continue with Acme.", { exact: false }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Create a new workspace", { exact: false }),
    ).not.toBeInTheDocument();

    const cta = screen.getByRole("button", { name: "Open Acme" });
    expect(cta).toBeEnabled();
  });
});
