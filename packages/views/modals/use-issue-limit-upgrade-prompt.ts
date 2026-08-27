"use client";

import { useCallback } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { workspaceSubscriptionSummaryOptions } from "@multica/core/billing";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";

interface IssueLimitUpgradePromptOptions {
  onNavigate?: () => void;
}

/**
 * Shows the issue-limit recovery action authorized by Cloud for the current
 * caller. The UI never infers billing permissions from a local workspace role:
 * only availableActions.checkout can expose the upgrade action.
 */
export function useIssueLimitUpgradePrompt(
  options: IssueLimitUpgradePromptOptions = {},
): () => Promise<void> {
  const { onNavigate } = options;
  const { t } = useT("modals");
  const queryClient = useQueryClient();
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();

  return useCallback(async () => {
    let checkoutAvailable: boolean | null = null;
    try {
      const summary = await queryClient.fetchQuery({
        ...workspaceSubscriptionSummaryOptions(wsId),
        // A quota rejection is a strong signal that the cached billing state
        // may be stale. Ask the server for the current per-caller action.
        staleTime: 0,
      });
      checkoutAvailable = summary?.availableActions.checkout ?? null;
    } catch {
      // Billing remains reachable as a neutral recovery path. Do not infer
      // checkout permission when Cloud's summary is unavailable.
    }

    const openBilling = () => {
      onNavigate?.();
      navigation.push(`${paths.settings()}?tab=billing`);
    };
    const title = t(($) => $.create_issue.issue_limit.title);

    if (checkoutAvailable === true) {
      toast.error(title, {
        description: t(($) => $.create_issue.issue_limit.upgrade_description),
        duration: 10_000,
        action: {
          label: t(($) => $.create_issue.issue_limit.upgrade_action),
          onClick: openBilling,
        },
      });
      return;
    }

    if (checkoutAvailable === false) {
      toast.error(title, {
        description: t(($) => $.create_issue.issue_limit.contact_description),
        duration: 10_000,
      });
      return;
    }

    toast.error(title, {
      description: t(($) => $.create_issue.issue_limit.billing_description),
      duration: 10_000,
      action: {
        label: t(($) => $.create_issue.issue_limit.billing_action),
        onClick: openBilling,
      },
    });
  }, [navigation, onNavigate, paths, queryClient, t, wsId]);
}
