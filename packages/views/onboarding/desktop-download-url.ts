/**
 * Where "Download the desktop app" links point.
 *
 * Upstream ContextPRO routes these to its own `/download` marketing page, which
 * detected the OS and listed every build. This fork removed the marketing
 * site, so the links go straight to the upstream GitHub releases page: the
 * desktop app is still built and published by upstream, and an installed
 * desktop client can point at any ContextPRO server, including this one.
 *
 * One constant rather than three literals so the login footer and both
 * onboarding steps can never drift to different destinations again.
 */
export const DESKTOP_DOWNLOAD_URL =
  "https://github.com/multica-ai/multica/releases/latest";
