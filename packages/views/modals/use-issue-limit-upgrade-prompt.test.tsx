import type { ReactNode } from "react";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";
import enModals from "../locales/en/modals.json";

const mockPush = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());
const summaryState = vi.hoisted(() => ({
  value: null as null | { availableActions: { checkout: boolean } },
  error: null as Error | null,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-test",
}));

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    settings: () => "/ws-test/settings",
  }),
}));

vi.mock("../navigation/context", () => ({
  useNavigation: () => ({ push: mockPush }),
}));

vi.mock("@multica/core/billing/workspace-subscription-queries", () => ({
  workspaceSubscriptionSummaryOptions: (wsId: string) => ({
    queryKey: ["workspace-subscriptions", wsId, "summary"],
    queryFn: async () => {
      if (summaryState.error) throw summaryState.error;
      return summaryState.value;
    },
  }),
}));

vi.mock("sonner", () => ({
  toast: {
    error: mockToastError,
  },
}));

import { useIssueLimitUpgradePrompt } from "./use-issue-limit-upgrade-prompt";

const TEST_RESOURCES = {
  en: { common: enCommon, modals: enModals },
};

function renderPrompt(onNavigate = vi.fn()) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </I18nProvider>
  );
  const hook = renderHook(
    () => useIssueLimitUpgradePrompt({ onNavigate }),
    { wrapper },
  );
  return { ...hook, onNavigate };
}

describe("useIssueLimitUpgradePrompt", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    summaryState.value = null;
    summaryState.error = null;
  });

  it("offers Upgrade to Pro only when Cloud authorizes checkout", async () => {
    summaryState.value = { availableActions: { checkout: true } };
    const { result, onNavigate } = renderPrompt();

    await act(async () => {
      await result.current();
    });

    expect(mockToastError).toHaveBeenCalledWith(
      "This workspace has reached its issue limit",
      expect.objectContaining({
        description: "Upgrade to Pro to keep creating issues.",
        action: expect.objectContaining({ label: "Upgrade to Pro" }),
      }),
    );
    const action = mockToastError.mock.calls[0]?.[1]?.action;
    action?.onClick();
    expect(onNavigate).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledWith("/ws-test/settings?tab=billing");
  });

  it("asks a member to contact owner or admin when Cloud denies checkout", async () => {
    summaryState.value = { availableActions: { checkout: false } };
    const { result } = renderPrompt();

    await act(async () => {
      await result.current();
    });

    expect(mockToastError).toHaveBeenCalledWith(
      "This workspace has reached its issue limit",
      expect.objectContaining({
        description: "Ask a workspace owner or admin to upgrade to Pro.",
      }),
    );
    expect(mockToastError.mock.calls[0]?.[1]).not.toHaveProperty("action");
  });

  it("uses a neutral Billing action when the Cloud summary is unavailable", async () => {
    summaryState.error = new Error("cloud unavailable");
    const { result } = renderPrompt();

    await act(async () => {
      await result.current();
    });

    expect(mockToastError).toHaveBeenCalledWith(
      "This workspace has reached its issue limit",
      expect.objectContaining({
        description: "Open Billing to see the upgrade options available for this workspace.",
        action: expect.objectContaining({ label: "View Billing" }),
      }),
    );
  });
});
