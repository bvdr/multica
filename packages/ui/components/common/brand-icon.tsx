import { useState, useEffect } from "react";
import { cn } from "../../lib/utils";

interface BrandIconProps extends React.ComponentProps<"span"> {
  /**
   * If true, play a one-time entrance spin animation.
   */
  animate?: boolean;
  /**
   * If true, disable hover spin animation.
   */
  noSpin?: boolean;
  /**
   * If true, show a border around the icon.
   */
  bordered?: boolean;
  /**
   * Size of the bordered icon: "sm" (default), "md", "lg"
   */
  size?: "sm" | "md" | "lg";
}

const borderedSizes = {
  sm: { wrapper: "p-1.5", icon: "size-3.5" },
  md: { wrapper: "p-2", icon: "size-4" },
  lg: { wrapper: "p-2.5", icon: "size-5" },
};

/**
 * ContextPRO mark: an open ring framing a dot — the thing in focus inside a
 * context window. Inline SVG in `currentColor` so it adapts to light/dark
 * themes automatically. The geometry is the single source of truth for the
 * mark and is mirrored in apps/web/public/favicon.svg, public/icons/icon.svg
 * and apps/mobile/components/brand/brand-logo.tsx — keep the four in sync.
 *
 * Kept the props API of the previous logo component (animate / noSpin /
 * bordered / size) so its call sites only needed the identifier rename.
 */
function Mark() {
  return (
    <svg viewBox="0 0 100 100" className="block size-full" aria-hidden="true">
      <path
        d="M 75.5 75.5 A 36 36 0 1 1 75.5 24.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="14"
        strokeLinecap="round"
      />
      <circle cx="60" cy="50" r="11" fill="currentColor" />
    </svg>
  );
}

export function BrandIcon({
  className,
  animate = false,
  noSpin = false,
  bordered = false,
  size = "sm",
  ...props
}: BrandIconProps) {
  const [entranceDone, setEntranceDone] = useState(!animate);

  useEffect(() => {
    if (!animate) return;
    const timer = setTimeout(() => setEntranceDone(true), 600);
    return () => clearTimeout(timer);
  }, [animate]);

  if (bordered) {
    const sizeConfig = borderedSizes[size];
    return (
      <span
        className={cn(
          "inline-flex items-center justify-center border border-border rounded-md",
          sizeConfig.wrapper,
          className
        )}
        aria-hidden="true"
        {...props}
      >
        <span
          className={cn(
            "block",
            sizeConfig.icon,
            !entranceDone && "animate-entrance-spin",
            entranceDone && !noSpin && "hover:animate-spin"
          )}
        >
          <Mark />
        </span>
      </span>
    );
  }

  return (
    <span
      className={cn(
        "inline-block size-[1em]",
        !entranceDone && "animate-entrance-spin",
        entranceDone && !noSpin && "hover:animate-spin",
        className
      )}
      aria-hidden="true"
      {...props}
    >
      <Mark />
    </span>
  );
}
