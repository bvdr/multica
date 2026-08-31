import type { DaemonState } from "../shared/daemon-types";

export const RECOVERY_STOPPED_THRESHOLD = 3;
export const RECOVERY_BACKOFF_MS = [15_000, 30_000, 120_000, 600_000] as const;

interface RecoveryObservation {
  desiredRunning: boolean;
  desktopManaged: boolean;
  lifecycleBusy: boolean;
  state: DaemonState;
  now: number;
}

/**
 * Pure policy for deciding when the Desktop should confirm and recover a lost
 * daemon. Side effects stay in daemon-manager; this class only owns the
 * consecutive-failure and capped-backoff state.
 */
export class DaemonRecoveryPolicy {
  private consecutiveStopped = 0;
  private failureCount = 0;
  private nextAttemptAt = 0;

  observe(observation: RecoveryObservation): boolean {
    const eligible =
      observation.desiredRunning &&
      observation.desktopManaged &&
      !observation.lifecycleBusy &&
      observation.state === "stopped";

    if (!eligible) {
      this.consecutiveStopped = 0;
      return false;
    }

    this.consecutiveStopped += 1;
    if (this.consecutiveStopped < RECOVERY_STOPPED_THRESHOLD) return false;

    this.consecutiveStopped = 0;
    return observation.now >= this.nextAttemptAt;
  }

  recordHealthy(): void {
    this.consecutiveStopped = 0;
    this.failureCount = 0;
    this.nextAttemptAt = 0;
  }

  recordFailure(now: number): void {
    const delay =
      RECOVERY_BACKOFF_MS[
        Math.min(this.failureCount, RECOVERY_BACKOFF_MS.length - 1)
      ];
    this.failureCount += 1;
    this.nextAttemptAt = now + delay;
  }

  reset(): void {
    this.recordHealthy();
  }
}

export function parseDaemonPid(raw: string): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const pid = Number(trimmed);
  return Number.isSafeInteger(pid) && pid > 0 ? pid : null;
}

/** Conservative process-liveness check: only ESRCH proves the PID is absent. */
export function daemonProcessExists(
  pid: number,
  signalProbe: (pid: number, signal: 0) => void = process.kill,
): boolean {
  try {
    signalProbe(pid, 0);
    return true;
  } catch (err) {
    return !(
      err &&
      typeof err === "object" &&
      "code" in err &&
      err.code === "ESRCH"
    );
  }
}
