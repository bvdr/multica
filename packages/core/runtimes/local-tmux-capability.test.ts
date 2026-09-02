// @vitest-environment node
import { describe, expect, it } from "vitest";
import { LOCAL_TMUX_CAPABILITY, runtimeAdvertisesLocalTmux, runtimeAdvertisesLocalWorktree } from "./cli-version";

describe("runtimeAdvertisesLocalTmux", () => {
  const rows = [
    { daemon_id: "d-1", last_seen_at: "2026-09-01T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
    { daemon_id: "d-1", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1", LOCAL_TMUX_CAPABILITY] } },
    { daemon_id: "d-2", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
  ];
  it("reads the newest row of the daemon", () => {
    expect(runtimeAdvertisesLocalTmux(rows, "d-1")).toBe(true);
    expect(runtimeAdvertisesLocalTmux(rows, "d-2")).toBe(false);
    expect(runtimeAdvertisesLocalTmux(rows, null)).toBe(false);
  });
  it("keeps the worktree helper's answer", () => {
    expect(runtimeAdvertisesLocalWorktree(rows, "d-2")).toBe(true);
  });
});
