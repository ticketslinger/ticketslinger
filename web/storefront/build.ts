import { cpSync, rmSync } from "node:fs";

// The stylesheet and the checkout island, side by side: the server serves both under /assets/.
// The event page ships no script of ours; only the checkout page loads checkout.js.
const out = process.argv[2] ?? "dist";
rmSync(out, { recursive: true, force: true });
cpSync("public", out, { recursive: true });
const r = await Bun.build({
  entrypoints: ["islands/checkout.tsx"],
  outdir: out,
  target: "browser",
  minify: true,
  naming: "[name].js",
});
if (!r.success) {
  for (const log of r.logs) console.error(log);
  process.exit(1);
}
