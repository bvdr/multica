import reactConfig from "@multica/eslint-config/react";
import i18next from "eslint-plugin-i18next";

// Global i18n protection. Every JSX text node in this package must pass
// through useT() — raw strings become a build error. Scope of
// `mode: "jsx-text-only"`: flags raw strings inside JSX children only;
// attribute values and plain TS literals are allowed through.

// The other half of the i18n surface (MUL-6850). User-visible copy reaches the
// screen through two doors the plugin never watches — a JSX attribute and a
// toast argument — which is why MUL-6838 had to hand-fix ~30 such strings after
// they had already shipped. Widening the plugin to `mode: "jsx-only"` is not
// the answer: measured on this package it reports 2639 errors, because it
// validates every literal that appears anywhere inside JSX (`isColVisible("status")`,
// `{ month: "short" }`, enum keys) and only ~10 of those are copy. So the two
// doors are pinned individually instead — same technique apps/desktop uses to
// guard shell.openExternal and router.navigate.
const NO_UNTRANSLATED_ATTRIBUTES = {
  // `https?:`-prefixed values are URLs and example endpoints, never copy.
  selector:
    "JSXAttribute[name.name=/^(placeholder|title|aria-label)$/] > Literal[value=/[A-Za-z]/]:not([value=/^https?:/])",
  message:
    "User-visible text in placeholder/title/aria-label must come from useT() — a screen reader reads aria-label the way a sighted user reads a JSX child. Exempt technical values (a CLI command, a token prefix) with an inline eslint-disable and a reason.",
};

const NO_UNTRANSLATED_TOAST = {
  // Both `toast("…")` and `toast.error("…")`.
  selector:
    ":matches(CallExpression[callee.name='toast'], CallExpression[callee.object.name='toast']) > Literal[value=/[A-Za-z]/]",
  message:
    "Toast copy must come from useT(). Pass a translated string, not a literal.",
};

export default [
  ...reactConfig,
  {
    files: ["**/*.tsx"],
    ignores: ["**/*.test.tsx", "test/**"],
    plugins: { i18next },
    rules: {
      "i18next/no-literal-string": [
        "error",
        { mode: "jsx-text-only" },
      ],
    },
  },
  {
    // Toasts are fired from hooks (`.ts`) as often as from components.
    files: ["**/*.{ts,tsx}"],
    ignores: ["**/*.test.{ts,tsx}", "test/**"],
    rules: {
      "no-restricted-syntax": [
        "error",
        NO_UNTRANSLATED_ATTRIBUTES,
        NO_UNTRANSLATED_TOAST,
      ],
    },
  },
];
