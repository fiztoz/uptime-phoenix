import js from "@eslint/js";
import globals from "globals";
import svelte from "eslint-plugin-svelte";
import ts from "typescript-eslint";

export default [
  {
    ignores: [
      ".svelte-kit/**",
      "build/**",
      "dist/**",
      "node_modules/**",
      "playwright-report/**",
      "src/lib/paraglide/**",
      "test-results/**",
    ],
  },
  js.configs.recommended,
  ...ts.configs.recommended,
  ...svelte.configs["flat/recommended"],
  ...svelte.configs["flat/prettier"],
  {
    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.node,
      },
    },
    rules: {
      "@typescript-eslint/no-empty-object-type": "off",
      "@typescript-eslint/no-explicit-any": "off",
      "@typescript-eslint/no-unused-vars": "off",
      // An absolute filesystem import resolves only in the checkout that
      // wrote it (#226). Use a relative path or the #lib alias.
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            {
              regex: "^(?:/|[A-Za-z]:[\\\\/]|file:)",
              message:
                "Import through a relative path or the #lib alias, not an absolute filesystem path.",
            },
          ],
        },
      ],
      "no-undef": "off",
      "no-useless-assignment": "off",
      "prefer-const": "off",
      "svelte/no-at-html-tags": "off",
      "svelte/no-navigation-without-resolve": "off",
      "svelte/no-unused-svelte-ignore": "off",
      "svelte/prefer-svelte-reactivity": "off",
      "svelte/require-each-key": "off",
    },
  },
  {
    files: ["**/*.svelte", "**/*.svelte.ts", "**/*.svelte.js"],
    languageOptions: {
      parserOptions: {
        parser: ts.parser,
      },
    },
  },
];
