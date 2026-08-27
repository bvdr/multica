import type { ReactNode } from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";
import enModals from "../locales/en/modals.json";

interface AvailableActions {
  checkout: boolean;
  portal: boolean;
  purchaseSeats: boolean;
}

const mockPush = vi.hoisted(() => vi.fn());
const mockCloseActiveModal = vi.hoisted(() => vi.fn());
const mockCreatePortal = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());
const mockSummaryQuery = vi.hoisted(() => vi.fn());
const featureState = vi.hoisted(() => ({ billingEnabled: true }));
const summaryState = vi.hoisted(() => ({
  value: null as null | { availableActions: AvailableActions },
  error: null as Error | null,
  pending: null as Promise<{ availableActions: AvailableActions } | null> | null,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-test",
}));

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    settings: () => "/ws-test/settings",
  }),
}));

vi.mock("@multica/core/config", () => ({
  useFeatureEnabled: () => featureState.billingEnabled,
}));

vi.mock("@multica/core/modals", () => ({
  useModalStore: {
    getState: () => ({ close: mockCloseActiveModal }),
  },
}));

vi.mock("../navigation/context", () => ({
  useNavigation: () => ({ push: mockPush }),
}));

vi.mock("../platform", () => ({
  openExternal: mockOpenExternal,
}));

vi.mock("@multica/core/billing", () => ({
  workspaceSubscriptionSummaryOptions: (wsId: string) => ({
    queryKey: ["workspace-subscriptions", wsId, "summary"],
    queryFn: mockSummaryQuery,
  }),
  useCreateWorkspaceSubscriptionPortal: () => ({
    mutateAsync: mockCreatePortal,
  }),
}));

vi.mock("@multica/ui/components/ui/dialog", () => ({
  Dialog: ({ open, children }: { open?: boolean; children: ReactNode }) =>
    open ? <div data-testid="issue-limit-dialog">{children}</div> : null,
  DialogContent: ({
    className,
    children,
  }: {
    className?: string;
    children: ReactNode;
  }) => (
    <section data-testid="issue-limit-dialog-content" className={className}>
      {children}
    </section>
  ),
  DialogDescription: ({
    children,
    ...props
  }: React.HTMLAttributes<HTMLParagraphElement>) => <p {...props}>{children}</p>,
  DialogHeader: ({
    children,
    ...props
  }: React.HTMLAttributes<HTMLDivElement>) => <div {...props}>{children}</div>,
  DialogTitle: ({
    children,
    ...props
  }: React.HTMLAttributes<HTMLHeadingElement>) => <h2 {...props}>{children}</h2>,
}));

import {
  IssueLimitUpgradeDialog,
  useIssueLimitUpgradePrompt,
} from "./use-issue-limit-upgrade-prompt";

const TEST_RESOURCES = {
  en: { common: enCommon, modals: enModals },
};

const actions = (
  overrides: Partial<AvailableActions> = {},
): AvailableActions => ({
  checkout: false,
  portal: false,
  purchaseSeats: false,
  ...overrides,
});

function PromptHarness() {
  const showPrompt = useIssueLimitUpgradePrompt();
  return (
    <>
      <button type="button" onClick={showPrompt}>
        Show recovery
      </button>
      <IssueLimitUpgradeDialog />
    </>
  );
}

function renderPrompt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: 1 } },
  });
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={client}>
        <PromptHarness />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { client };
}

async function openPrompt() {
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Show recovery" }));
  return user;
}

describe("IssueLimitUpgradeDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    featureState.billingEnabled = true;
    summaryState.value = null;
    summaryState.error = null;
    summaryState.pending = null;
    mockSummaryQuery.mockImplementation(async () => {
      if (summaryState.pending) return summaryState.pending;
      if (summaryState.error) throw summaryState.error;
      return summaryState.value;
    });
    mockCreatePortal.mockResolvedValue({
      url: "https://billing.example/portal",
    });
  });

  afterEach(() => {
    const close = screen.queryByRole("button", { name: "Close" });
    if (close) fireEvent.click(close);
    cleanup();
  });

  it("opens immediately as a spacious centered recovery dialog", async () => {
    summaryState.pending = new Promise<{
      availableActions: AvailableActions;
    } | null>(() => undefined);
    renderPrompt();

    await openPrompt();

    expect(
      screen.getByRole("heading", {
        name: "This workspace has reached its issue limit",
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Checking the billing actions available for this workspace…",
      ),
    ).toBeInTheDocument();
    expect(screen.getByTestId("issue-limit-dialog-content")).toHaveClass(
      "sm:max-w-lg",
      "overflow-hidden",
      "p-0",
    );
  });

  it("stays dismissed when the Cloud response arrives later", async () => {
    let resolveSummary!: (
      value: { availableActions: AvailableActions } | null,
    ) => void;
    summaryState.pending = new Promise((resolve) => {
      resolveSummary = resolve;
    });
    renderPrompt();
    const user = await openPrompt();

    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByTestId("issue-limit-dialog")).not.toBeInTheDocument();

    await act(async () => {
      resolveSummary({ availableActions: actions({ checkout: true }) });
      await summaryState.pending;
    });

    expect(screen.queryByTestId("issue-limit-dialog")).not.toBeInTheDocument();
  });

  it("keeps the create modal open when recovery is merely dismissed", async () => {
    summaryState.value = { availableActions: actions({ checkout: true }) };
    renderPrompt();
    const user = await openPrompt();

    await user.click(screen.getByRole("button", { name: "Close" }));

    expect(mockCloseActiveModal).not.toHaveBeenCalled();
  });

  it("offers Upgrade to Pro only when Cloud authorizes checkout", async () => {
    summaryState.value = { availableActions: actions({ checkout: true }) };
    renderPrompt();
    const user = await openPrompt();

    await user.click(
      await screen.findByRole("button", { name: "Upgrade to Pro" }),
    );

    expect(mockCloseActiveModal).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledWith("/ws-test/settings?tab=billing");
    expect(screen.queryByTestId("issue-limit-dialog")).not.toBeInTheDocument();
  });

  it("opens Billing Portal for a past-due manager authorized for portal", async () => {
    summaryState.value = { availableActions: actions({ portal: true }) };
    renderPrompt();
    const user = await openPrompt();

    await user.click(
      await screen.findByRole("button", { name: "Open Billing Portal" }),
    );

    await waitFor(() => expect(mockCreatePortal).toHaveBeenCalledTimes(1));
    expect(mockCreatePortal.mock.calls[0]?.[0]).toMatch(
      /^issue-limit-portal-ws-test-/,
    );
    await waitFor(() => {
      expect(mockOpenExternal).toHaveBeenCalledWith(
        "https://billing.example/portal",
        { webTarget: "same-tab" },
      );
    });
    expect(mockCloseActiveModal).toHaveBeenCalledTimes(1);
  });

  it("keeps a Billing recovery action when Portal cannot be opened", async () => {
    summaryState.value = { availableActions: actions({ portal: true }) };
    mockCreatePortal.mockRejectedValue(new Error("portal unavailable"));
    renderPrompt();
    const user = await openPrompt();

    await user.click(
      await screen.findByRole("button", { name: "Open Billing Portal" }),
    );

    expect(
      await screen.findByRole("button", { name: "View Billing" }),
    ).toBeInTheDocument();
    expect(mockCloseActiveModal).not.toHaveBeenCalled();
    expect(screen.getByTestId("issue-limit-dialog")).toBeInTheDocument();
  });

  it("asks for an administrator only when Cloud authorizes no management action", async () => {
    summaryState.value = { availableActions: actions() };
    renderPrompt();
    await openPrompt();

    expect(
      await screen.findByText(
        "Ask a workspace owner or admin to upgrade to Pro.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", {
        name: /Upgrade|Billing Portal|View Billing/,
      }),
    ).not.toBeInTheDocument();
  });

  it("keeps another Cloud-authorized management action reachable in Billing", async () => {
    summaryState.value = {
      availableActions: actions({ purchaseSeats: true }),
    };
    renderPrompt();
    await openPrompt();

    expect(
      await screen.findByRole("button", { name: "View Billing" }),
    ).toBeInTheDocument();
  });

  it("uses one background attempt and keeps Billing as the recovery path", async () => {
    summaryState.error = new Error("cloud unavailable");
    renderPrompt();
    await openPrompt();

    expect(
      await screen.findByRole("button", { name: "View Billing" }),
    ).toBeInTheDocument();
    expect(mockSummaryQuery).toHaveBeenCalledTimes(1);
  });

  it("does not expose a dead Billing link when the Billing surface is disabled", async () => {
    featureState.billingEnabled = false;
    renderPrompt();
    await openPrompt();

    expect(
      screen.getByText(
        "Delete an existing issue to free space, or contact your workspace administrator.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "View Billing" }),
    ).not.toBeInTheDocument();
    expect(mockSummaryQuery).not.toHaveBeenCalled();
  });
});
