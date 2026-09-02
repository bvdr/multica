"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useConfigStore } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import { runtimeAdvertisesLocalTmux, runtimeAdvertisesLocalWorktree } from "@multica/core/runtimes";
import type { LocalDirectoryExecutionMode, LocalDirectoryResourceRef, Workspace } from "@multica/core/types";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useT } from "../../i18n";
import {
  LocalDirectoryModeOptions,
  type TmuxUnavailableReason,
  type WorktreeUnavailableReason,
} from "../../projects/components/local-directory-mode-dialog";
import { SettingsCard, SettingsSection } from "./settings-layout";

/**
 * Workspace-wide fallback folder (ContextPRO fork). Saved through the normal
 * workspace update; the server validates the ref like a project resource and
 * gates worktree/tmux on the runtime's capability, so the picker only offers
 * what the chosen machine can run. Settings saves await the server (no
 * optimistic update): the value is a shared configuration, not a toggle.
 */
interface DefaultLocalDirectorySectionProps {
  /** Same rule as the rest of the Repositories tab: owners and admins edit, members read. */
  canManage?: boolean;
}

export function DefaultLocalDirectorySection({ canManage = true }: DefaultLocalDirectorySectionProps) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const workspace = useCurrentWorkspace();
  const queryClient = useQueryClient();
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const serverValidatesWorktree = useConfigStore((s) => s.localWorktreeSupported);
  const serverValidatesTmux = useConfigStore((s) => s.localTmuxSupported);

  const saved = workspace?.default_local_directory ?? null;
  const [daemonId, setDaemonId] = useState<string>(saved?.daemon_id ?? "");
  const [path, setPath] = useState<string>(saved?.local_path ?? "");
  const [mode, setMode] = useState<LocalDirectoryExecutionMode>(saved?.execution_mode ?? "in_place");
  const [saving, setSaving] = useState(false);

  // Re-seed the form when another client changes the saved value.
  useEffect(() => {
    setDaemonId(saved?.daemon_id ?? "");
    setPath(saved?.local_path ?? "");
    setMode(saved?.execution_mode ?? "in_place");
  }, [saved?.daemon_id, saved?.local_path, saved?.execution_mode]);

  const runtimeChoices = useMemo(
    () => runtimes.filter((rt) => typeof rt.daemon_id === "string" && rt.daemon_id !== ""),
    [runtimes],
  );
  // The dialog has no "runtime cannot" reason for worktree (that is the
  // server's call on save), so a runtime without the capability reuses the
  // not_git reason: either way the user is told to pick another mode.
  const worktreeUnavailable: WorktreeUnavailableReason | undefined = !serverValidatesWorktree
    ? "server_outdated"
    : runtimeAdvertisesLocalWorktree(runtimes, daemonId || null)
      ? undefined
      : "not_git";
  const tmuxUnavailable: TmuxUnavailableReason | undefined = !serverValidatesTmux
    ? "server_outdated"
    : runtimeAdvertisesLocalTmux(runtimes, daemonId || null)
      ? undefined
      : "runtime_no_tmux";

  // Mirrors the server's isAbsoluteLocalPath: POSIX "/..." or Windows "C:\...".
  const pathIsAbsolute = /^\/|^[A-Za-z]:[\\/]/.test(path.trim());
  const canSave = canManage && !!workspace && daemonId !== "" && pathIsAbsolute && !saving;

  const applyUpdated = (updated: Workspace) => {
    queryClient.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
      old?.map((item) => (item.id === updated.id ? updated : item)),
    );
  };

  const save = async () => {
    if (!workspace || !canSave) return;
    const ref: LocalDirectoryResourceRef = { local_path: path.trim(), daemon_id: daemonId, execution_mode: mode };
    setSaving(true);
    try {
      applyUpdated(await api.updateWorkspace(workspace.id, { default_local_directory: ref }));
      toast.success(t(($) => $.repositories.default_folder_saved));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.repositories.default_folder_save_failed));
    } finally {
      setSaving(false);
    }
  };

  const clear = async () => {
    if (!workspace) return;
    setSaving(true);
    try {
      applyUpdated(await api.updateWorkspace(workspace.id, { default_local_directory: null }));
      toast.success(t(($) => $.repositories.default_folder_cleared));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.repositories.default_folder_save_failed));
    } finally {
      setSaving(false);
    }
  };

  if (!workspace) return null;

  return (
    <SettingsSection
      title={t(($) => $.repositories.default_folder_title)}
      description={t(($) => $.repositories.default_folder_description)}
    >
      <SettingsCard>
        <div className="flex flex-col gap-4 p-4">
          {saved === null && (
            <p className="text-caption text-muted-foreground">{t(($) => $.repositories.default_folder_none)}</p>
          )}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="default-folder-runtime">{t(($) => $.repositories.default_folder_runtime)}</Label>
            {/* Native select: a handful of machines, and it keeps the test and
                keyboard story simple. */}
            <select
              id="default-folder-runtime"
              className="h-9 rounded-md border border-input bg-background px-3 text-body"
              value={daemonId}
              disabled={!canManage}
              onChange={(e) => setDaemonId(e.target.value)}
            >
              <option value="">{t(($) => $.repositories.default_folder_runtime_placeholder)}</option>
              {runtimeChoices.map((rt) => (
                <option key={rt.id} value={rt.daemon_id ?? ""}>
                  {rt.name}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="default-folder-path">{t(($) => $.repositories.default_folder_path)}</Label>
            <Input
              id="default-folder-path"
              value={path}
              disabled={!canManage}
              placeholder={t(($) => $.repositories.default_folder_path_placeholder)}
              onChange={(e) => setPath(e.target.value)}
            />
            <p className="text-micro text-muted-foreground">{t(($) => $.repositories.default_folder_path_hint)}</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t(($) => $.repositories.default_folder_mode)}</Label>
            {canManage ? (
              <LocalDirectoryModeOptions
                value={mode}
                onChange={setMode}
                unavailableReason={worktreeUnavailable}
                tmuxUnavailableReason={tmuxUnavailable}
              />
            ) : (
              <p className="text-caption text-muted-foreground">{mode}</p>
            )}
          </div>
          <div className="flex items-center gap-2">
            <Button onClick={() => void save()} disabled={!canSave}>
              {t(($) => $.repositories.default_folder_save)}
            </Button>
            <Button variant="outline" onClick={() => void clear()} disabled={!canManage || saving || saved === null}>
              {t(($) => $.repositories.default_folder_clear)}
            </Button>
          </div>
        </div>
      </SettingsCard>
    </SettingsSection>
  );
}
