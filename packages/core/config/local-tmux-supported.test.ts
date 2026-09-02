// @vitest-environment node
import { describe, expect, it } from "vitest";
import { configStore } from "./index";

describe("localTmuxSupported", () => {
  it("defaults to false and only accepts an explicit true", () => {
    expect(configStore.getState().localTmuxSupported).toBe(false);
    configStore.getState().setLocalTmuxSupported(true);
    expect(configStore.getState().localTmuxSupported).toBe(true);
    // An absent or non-boolean value from an old server must fail closed.
    configStore.getState().setLocalTmuxSupported(undefined);
    expect(configStore.getState().localTmuxSupported).toBe(false);
  });
});
