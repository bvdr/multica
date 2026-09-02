// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import enProjects from "../../locales/en/projects.json";

const mockUpdateWorkspace = vi.hoisted(() => vi.fn());
const workspaceRef = vi.hoisted(() => ({
  current: {
    id: "workspace-1",
    name: "Test Workspace",
    slug: "test-workspace",
    repos: [] as { url: string }[],
    default_local_directory: null as null | { local_path: string; daemon_id: string; execution_mode?: string; label?: string },
  },
}));
const runtimesRef = vi.hoisted(() => ({
  current: [
    { id: "rt-1", name: "Mac mini (Work)", daemon_id: "daemon-work", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1", "local-tmux-v1"] } },
    { id: "rt-2", name: "Laptop", daemon_id: "daemon-laptop", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
  ],
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: runtimesRef.current }),
  useQueryClient: () => ({ setQueryData: vi.fn() }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => workspaceRef.current }));
vi.mock("@multica/core/workspace/queries", () => ({ workspaceKeys: { list: () => ["workspaces"] } }));
vi.mock("@multica/core/runtimes/queries", () => ({ runtimeListOptions: () => ({ queryKey: ["runtimes"] }) }));
vi.mock("@multica/core/api", () => ({ api: { updateWorkspace: mockUpdateWorkspace } }));
vi.mock("@multica/core/config", () => {
  const state = { localTmuxSupported: true, localWorktreeSupported: true };
  const useConfigStore = Object.assign((selector: (s: typeof state) => unknown) => selector(state), { getState: () => state });
  return { useConfigStore };
});
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { DefaultLocalDirectorySection } from "./default-local-directory-section";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings, projects: enProjects } };
function renderSection() {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <DefaultLocalDirectorySection />
    </I18nProvider>,
  );
}

describe("DefaultLocalDirectorySection", () => {
  beforeEach(() => {
    mockUpdateWorkspace.mockReset();
    mockUpdateWorkspace.mockImplementation(async (_id: string, data: { default_local_directory: unknown }) => ({
      ...workspaceRef.current,
      default_local_directory: data.default_local_directory,
    }));
    workspaceRef.current.default_local_directory = null;
  });

  it("saves a default folder with the chosen runtime and mode", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-work");
    await user.type(screen.getByLabelText("Folder path"), "/Users/dev/contextpro");
    await user.click(screen.getByRole("radio", { name: /interactive terminal/i }));
    await user.click(screen.getByRole("button", { name: "Save default folder" }));
    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledTimes(1));
    expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
      default_local_directory: { local_path: "/Users/dev/contextpro", daemon_id: "daemon-work", execution_mode: "tmux" },
    });
  });

  it("disables tmux for a runtime without the capability", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-laptop");
    expect(screen.getByRole("radio", { name: /interactive terminal/i }).hasAttribute("disabled")).toBe(true);
  });

  it("shows the saved default and clears it with null", async () => {
    workspaceRef.current.default_local_directory = { local_path: "/srv/app", daemon_id: "daemon-work", execution_mode: "in_place" };
    const user = userEvent.setup();
    renderSection();
    expect(screen.getByDisplayValue("/srv/app")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", { default_local_directory: null }));
  });

  it("keeps Save disabled until the path is absolute", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-work");
    await user.type(screen.getByLabelText("Folder path"), "relative/dir");
    expect(screen.getByRole("button", { name: "Save default folder" })).toBeDisabled();
  });
});
