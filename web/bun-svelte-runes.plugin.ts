/**
 * Bun test plugin: compile `.svelte.ts` rune modules before importing them.
 *
 * Runes are compiler syntax, not runtime functions — `bun test` importing a
 * `.svelte.ts` module raw dies with "ReferenceError: $state is not defined".
 * Vite handles this in dev/build via the Svelte plugin; `bun test` does not go
 * through Vite, so it needs the same transform.
 *
 * Preloaded for `bun test` only (see bunfig.toml); the Vite build is unaffected.
 */
import { plugin, Transpiler } from "bun";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { compileModule } from "svelte/compiler";

const ts = new Transpiler({ loader: "ts" });
const libDir = join(import.meta.dir, "src", "lib");

/**
 * Resolve a `#lib/...` specifier (package.json "imports") to its source file.
 * Sources import TS modules with a `.js` extension; Vite maps that to `.ts`,
 * but Bun on Linux does not apply that fallback to subpath imports.
 */
function resolveLib(specifier: string): string {
  const target = join(libDir, specifier.slice("#lib/".length));
  const candidates = target.endsWith(".js")
    ? [target, target.slice(0, -3) + ".ts"]
    : [target, target + ".ts", join(target, "index.ts")];
  return candidates.find((p) => existsSync(p)) ?? target;
}

plugin({
  name: "svelte-runes-modules",
  setup(build) {
    build.onResolve({ filter: /^#lib\// }, ({ path }) => ({
      path: resolveLib(path),
    }));
    build.onLoad({ filter: /\.svelte\.ts$/ }, ({ path }) => {
      // Strip TS types first: compileModule parses JS, so a generic call
      // like `$state<Foo | null>(null)` would be a syntax error to it.
      const js = ts.transformSync(readFileSync(path, "utf8"));
      const compiled = compileModule(js, {
        filename: path,
        generate: "client",
      });
      return { contents: compiled.js.code, loader: "js" };
    });
  },
});
