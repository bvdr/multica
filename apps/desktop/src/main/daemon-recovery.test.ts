// @vitest-environment node
import { describe, expect, it, vi } from "vitest";

import {
  DaemonRecoveryPolicy,
  RECOVERY_BACKOFF_MS,
  daemonProcessExists,
  parseDaemonPid,
} from "./daemon-recovery";

const eligible = {
  desiredRunning: true,
  desktopManaged: true,
  lifecycleBusy: false,
  state: "stopped" as const,
  now: 1_000,
};

describe("DaemonRecoveryPolicy", () => {
  it("requires three consecutive stopped polls before recovery", () => {
    const policy = new DaemonRecoveryPolicy();
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(true);
  });

  it("resets the stopped streak when a daemon responds", () => {
    const policy = new DaemonRecoveryPolicy();
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe({ ...eligible, state: "running" })).toBe(false);
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(true);
  });

  it.each([
    { desiredRunning: false },
    { desktopManaged: false },
    { lifecycleBusy: true },
    { state: "running" as const },
    { state: "starting" as const },
    { state: "stopping" as const },
    { state: "installing_cli" as const },
    { state: "cli_not_found" as const },
    { state: "auth_expired" as const },
  ])("does not recover when ineligible: %o", (override) => {
    const policy = new DaemonRecoveryPolicy();
    for (let i = 0; i < 5; i += 1) {
      expect(policy.observe({ ...eligible, ...override })).toBe(false);
    }
  });

  it("backs off failed attempts and caps the delay", () => {
    const policy = new DaemonRecoveryPolicy();
    const reachAttempt = (now: number): boolean => {
      policy.observe({ ...eligible, now });
      policy.observe({ ...eligible, now });
      return policy.observe({ ...eligible, now });
    };

    expect(reachAttempt(1_000)).toBe(true);
    policy.recordFailure(1_000);
    expect(reachAttempt(1_000 + RECOVERY_BACKOFF_MS[0] - 1)).toBe(false);
    expect(reachAttempt(1_000 + RECOVERY_BACKOFF_MS[0])).toBe(true);

    let now = 1_000 + RECOVERY_BACKOFF_MS[0];
    for (let failure = 1; failure < RECOVERY_BACKOFF_MS.length + 2; failure += 1) {
      policy.recordFailure(now);
      const delay =
        RECOVERY_BACKOFF_MS[
          Math.min(failure, RECOVERY_BACKOFF_MS.length - 1)
        ];
      expect(reachAttempt(now + delay - 1)).toBe(false);
      now += delay;
      expect(reachAttempt(now)).toBe(true);
    }
  });

  it("clears backoff after a healthy observation", () => {
    const policy = new DaemonRecoveryPolicy();
    policy.recordFailure(1_000);
    policy.recordHealthy();
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(false);
    expect(policy.observe(eligible)).toBe(true);
  });
});

describe("daemon PID checks", () => {
  it.each([
    ["42", 42],
    ["42\n", 42],
    ["", null],
    ["0", null],
    ["-1", null],
    ["12x", null],
  ])("parses %j as %j", (raw, expected) => {
    expect(parseDaemonPid(raw)).toBe(expected);
  });

  it("treats only ESRCH as proof that a PID is absent", () => {
    const alive = vi.fn();
    expect(daemonProcessExists(42, alive)).toBe(true);
    expect(alive).toHaveBeenCalledWith(42, 0);

    expect(
      daemonProcessExists(42, () => {
        throw Object.assign(new Error("gone"), { code: "ESRCH" });
      }),
    ).toBe(false);
    expect(
      daemonProcessExists(42, () => {
        throw Object.assign(new Error("denied"), { code: "EPERM" });
      }),
    ).toBe(true);
  });
});
