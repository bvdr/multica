"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Gauge, Loader2 } from "lucide-react";
import { create } from "zustand";
import {
  useCreateWorkspaceSubscriptionPortal,
  workspaceSubscriptionSummaryOptions,
} from "@multica/core/billing";
import { useFeatureEnabled } from "@multica/core/config";
import { BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG } from "@multica/core/feature-flags";
import { useWorkspaceId } from "@multica/core/hooks";
import { useModalStore } from "@multica/core/modals";
import { useWorkspacePaths } from "@multica/core/paths";
import type { WorkspaceSubscriptionSummary } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";
import { openExternal } from "../platform";

type BillingActions = WorkspaceSubscriptionSummary["availableActions"];
type ModalsT = ReturnType<typeof useT<"modals">>["t"];

interface IssueLimitRecoveryStore {
  visible: boolean;
  show: () => void;
  dismiss: () => void;
}

const useIssueLimitRecoveryStore = create<IssueLimitRecoveryStore>((set) => ({
  visible: false,
  show: () => set({ visible: true }),
  dismiss: () => set({ visible: false }),
}));

function createPortalIdempotencyKey(wsId: string): string {
  const suffix =
    globalThis.crypto?.randomUUID?.() ??
    `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `issue-limit-portal-${wsId}-${suffix}`.slice(0, 255);
}

/** Opens the shared issue-limit recovery dialog without closing the current draft. */
export function useIssueLimitUpgradePrompt(): () => void {
  return useCallback(() => {
    useIssueLimitRecoveryStore.getState().show();
  }, []);
}

/**
 * Centered recovery surface shared by manual create, Quick Create, and Inbox.
 * Cloud's complete action set is the only input that decides which billing
 * action appears; no local role, plan, subscription, or quota inference does.
 */
export function IssueLimitUpgradeDialog() {
  const visible = useIssueLimitRecoveryStore((state) => state.visible);
  const dismiss = useIssueLimitRecoveryStore((state) => state.dismiss);
  const { t } = useT("modals");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const billingEnabled = useFeatureEnabled(
    BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG,
    false,
  );
  const summaryQuery = useQuery({
    ...workspaceSubscriptionSummaryOptions(wsId),
    enabled: visible && billingEnabled,
    staleTime: 0,
    retry: false,
  });
  const createPortal = useCreateWorkspaceSubscriptionPortal(wsId).mutateAsync;
  const portalIntentKeyRef = useRef<string | null>(null);
  const [openingPortal, setOpeningPortal] = useState(false);
  const [portalFailed, setPortalFailed] = useState(false);

  useEffect(() => {
    if (!visible) return;
    setOpeningPortal(false);
    setPortalFailed(false);
  }, [visible]);

  const closeForBillingAction = () => {
    dismiss();
    // The recovery dialog is intentionally independent from ModalRegistry so
    // it can sit above a create dialog. Following a billing action closes that
    // underlying dialog; merely dismissing recovery leaves the draft visible.
    useModalStore.getState().close();
  };
  const openBilling = () => {
    closeForBillingAction();
    navigation.push(`${paths.settings()}?tab=billing`);
  };
  const openPortal = async () => {
    const key =
      portalIntentKeyRef.current ?? createPortalIdempotencyKey(wsId);
    portalIntentKeyRef.current = key;
    setOpeningPortal(true);
    try {
      const response = await createPortal(key);
      if (!response?.url) {
        setPortalFailed(true);
        return;
      }
      portalIntentKeyRef.current = null;
      closeForBillingAction();
      openExternal(response.url, { webTarget: "same-tab" });
    } catch {
      setPortalFailed(true);
    } finally {
      setOpeningPortal(false);
    }
  };

  const actions = summaryQuery.data?.availableActions;
  const recovery = resolveRecoveryPresentation({
    actions,
    billingEnabled,
    loading: billingEnabled && !actions && summaryQuery.isFetching,
    portalFailed,
    t,
    openBilling,
    openPortal,
  });

  return (
    <Dialog
      open={visible}
      modal
      onOpenChange={(open) => {
        if (!open) dismiss();
      }}
    >
      <DialogContent
        className="gap-0 overflow-hidden p-0 sm:max-w-lg"
        aria-describedby="issue-limit-recovery-description"
      >
        <div className="px-6 pb-7 pt-8 text-center sm:px-10 sm:pb-8 sm:pt-10">
          <div className="mx-auto flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary ring-1 ring-primary/15">
            <Gauge className="size-7" aria-hidden="true" />
          </div>
          <DialogHeader className="mt-5 items-center gap-3 text-center">
            <DialogTitle className="max-w-md text-balance text-display-sm font-semibold leading-tight tracking-tight">
              {t(($) => $.create_issue.issue_limit.title)}
            </DialogTitle>
            <DialogDescription
              id="issue-limit-recovery-description"
              className="max-w-sm text-pretty text-center text-body leading-6"
            >
              {recovery.description}
            </DialogDescription>
          </DialogHeader>
        </div>

        <div className="flex flex-col-reverse gap-2 border-t border-surface-border bg-surface-hover/60 px-6 py-4 sm:flex-row sm:justify-center sm:px-10">
          <Button
            variant={recovery.action ? "outline" : "default"}
            size="lg"
            className="sm:min-w-28"
            onClick={dismiss}
          >
            {t(($) => $.close)}
          </Button>
          {recovery.action && (
            <Button
              size="lg"
              className="sm:min-w-40"
              disabled={openingPortal}
              onClick={recovery.action.onClick}
            >
              {openingPortal && <Loader2 className="animate-spin" />}
              {recovery.action.label}
            </Button>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

interface RecoveryPresentation {
  description: string;
  action?: {
    label: string;
    onClick: () => void;
  };
}

function resolveRecoveryPresentation({
  actions,
  billingEnabled,
  loading,
  portalFailed,
  t,
  openBilling,
  openPortal,
}: {
  actions: BillingActions | undefined;
  billingEnabled: boolean;
  loading: boolean;
  portalFailed: boolean;
  t: ModalsT;
  openBilling: () => void;
  openPortal: () => Promise<void>;
}): RecoveryPresentation {
  if (!billingEnabled) {
    return {
      description: t(
        ($) => $.create_issue.issue_limit.billing_disabled_description,
      ),
    };
  }

  if (portalFailed) {
    return {
      description: t(
        ($) => $.create_issue.issue_limit.billing_unavailable_description,
      ),
      action: {
        label: t(($) => $.create_issue.issue_limit.billing_action),
        onClick: openBilling,
      },
    };
  }

  if (actions?.checkout) {
    return {
      description: t(($) => $.create_issue.issue_limit.upgrade_description),
      action: {
        label: t(($) => $.create_issue.issue_limit.upgrade_action),
        onClick: openBilling,
      },
    };
  }

  if (actions?.portal) {
    return {
      description: t(($) => $.create_issue.issue_limit.portal_description),
      action: {
        label: t(($) => $.create_issue.issue_limit.portal_action),
        onClick: () => {
          void openPortal();
        },
      },
    };
  }

  if (actions?.purchaseSeats) {
    return {
      description: t(($) => $.create_issue.issue_limit.billing_description),
      action: {
        label: t(($) => $.create_issue.issue_limit.billing_action),
        onClick: openBilling,
      },
    };
  }

  if (actions) {
    return {
      description: t(($) => $.create_issue.issue_limit.contact_description),
    };
  }

  if (loading) {
    return {
      description: t(($) => $.create_issue.issue_limit.checking_description),
    };
  }

  return {
    description: t(
      ($) => $.create_issue.issue_limit.billing_unavailable_description,
    ),
    action: {
      label: t(($) => $.create_issue.issue_limit.billing_action),
      onClick: openBilling,
    },
  };
}
