import { cpSync, rmSync } from "node:fs";

// The stylesheet and the two islands, side by side: the server serves them under /assets/.
// The event page loads only turnstile.js, which fetches Turnstile once the buyer starts on the form.
const out = process.argv[2] ?? "dist";
rmSync(out, { recursive: true, force: true });
cpSync("public", out, { recursive: true });
const r = await Bun.build({
  entrypoints: ["islands/checkout.tsx", "islands/turnstile.ts"],
  outdir: out,
  target: "browser",
  minify: true,
  naming: "[name].js",
});
if (!r.success) {
  for (const log of r.logs) console.error(log);
  process.exit(1);
}
