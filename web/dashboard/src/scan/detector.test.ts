import { expect, test } from "bun:test";
import { mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const root = join(import.meta.dir, "../..");

test("the entry chunk doesn't carry the decoder", () => {
  const out = mkdtempSync(join(tmpdir(), "dash-build-"));
  try {
    const run = Bun.spawnSync(
      [
        join(root, "node_modules/.bin/vite"),
        "build",
        "--manifest",
        "--outDir",
        out,
        "--emptyOutDir",
      ],
      { cwd: root, stdout: "pipe", stderr: "pipe" },
    );
    expect(run.exitCode).toBe(0);
    const manifest = JSON.parse(readFileSync(join(out, ".vite/manifest.json"), "utf8")) as Record<
      string,
      { file: string; isEntry?: boolean }
    >;
    const entry = Object.values(manifest).find((c) => c.isEntry);
    expect(entry).toBeDefined();
    expect(readFileSync(join(out, entry?.file ?? ""), "utf8")).not.toContain("zxing");
    expect(readdirSync(join(out, "assets")).some((f) => f.endsWith(".wasm"))).toBe(true);
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
}, 120_000);
