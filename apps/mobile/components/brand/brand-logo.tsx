/**
 * ContextPRO mark: an open ring framing a dot. Vector copy of the shared mark
 * in packages/ui/components/common/brand-icon.tsx (and apps/web/public/
 * favicon.svg) — keep the geometry in sync with those files.
 *
 * react-native-svg does not resolve CSS `currentColor`, so callers must pass
 * `color` explicitly. For theme-aware usage, pair with `useColorScheme` +
 * `THEME` token from `@/lib/theme`.
 */
import Svg, { Circle, Path } from "react-native-svg";
import { THEME } from "@/lib/theme";
import { useColorScheme } from "@/lib/use-color-scheme";

interface BrandLogoProps {
  size?: number;
  color?: string;
}

export function BrandLogo({ size = 48, color }: BrandLogoProps) {
  const { isDarkColorScheme } = useColorScheme();
  const resolvedColor =
    color ?? (isDarkColorScheme ? THEME.dark.foreground : THEME.light.foreground);

  return (
    <Svg width={size} height={size} viewBox="0 0 100 100">
      <Path
        d="M 75.5 75.5 A 36 36 0 1 1 75.5 24.5"
        fill="none"
        stroke={resolvedColor}
        strokeWidth={14}
        strokeLinecap="round"
      />
      <Circle cx={60} cy={50} r={11} fill={resolvedColor} />
    </Svg>
  );
}
