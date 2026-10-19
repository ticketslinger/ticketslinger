import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { gzipSync } from "node:zlib";
import { createApp } from "./app";

const root = join(import.meta.dir, "..");

test("the checkout island stays under 20 KB gzipped, the Turnstile loader under 1 KB", () => {
  const out = mkdtempSync(join(tmpdir(), "storefront-build-"));
  try {
    const r = Bun.spawnSync(["bun", "build.ts", out], { cwd: root, stderr: "pipe" });
    expect(r.exitCode).toBe(0);
    const size = gzipSync(readFileSync(join(out, "checkout.js"))).length;
    expect(size).toBeLessThanOrEqual(20 * 1024);
    // The event page's whole first-load script of ours.
    expect(gzipSync(readFileSync(join(out, "turnstile.js"))).length).toBeLessThanOrEqual(1024);
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
}, 30_000);

test("the event page loads only the Turnstile loader, and Turnstile itself waits", async () => {
  const event = {
    organizer: { name: "Acme", slug: "acme" },
    event: {
      id: "e1",
      slug: "gala",
      name: "Gala",
      description: "",
      venue_name: "",
      venue_address: "",
      timezone: "UTC",
      starts_at: "2099-01-01T00:00:00Z",
      ends_at: "2099-01-01T04:00:00Z",
      fee_mode: "pass",
      state: "on_sale",
    },
    ticket_types: [
      {
        id: "t1",
        name: "General",
        description: "",
        price_cents: 2500,
        min_per_order: 1,
        max_per_order: 10,
        remaining: 3,
        on_sale: true,
        sales_start: null,
        sales_end: null,
        unlocked: false,
      },
    ],
    promo: null,
    questions: [],
  };
  const app = createApp({
    apiUrl: "http://api/v1",
    staticDir: "./dist",
    turnstileSiteKey: "1x00000000000000000000AA",
    peerIp: () => "127.0.0.1",
    fetch: async () => Response.json(event),
  });
  const html = await (await app.request("/acme/gala")).text();
  expect(html).toContain("Gala");
  expect(html.match(/src="[^"]*"/g)).toEqual(['src="/assets/turnstile.js"']);
  expect(html).not.toContain("challenges.cloudflare.com");
});
