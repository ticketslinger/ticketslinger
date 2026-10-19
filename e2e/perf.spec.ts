import { expect, test } from "@playwright/test";
import { createSellableEvent, signUpWithPasskey, virtualAuthenticator } from "./helpers";
import { lighthouseScores, scriptBytes } from "./perf";

// Set: production mode (P5b launch-check), no local stack, three Lighthouse runs, performance 1.
const PERF_EVENT_URL = process.env.PERF_EVENT_URL;

test("the event page stays small and fast", async ({ page, context }) => {
  // Lighthouse alone outlasts the 30 s default.
  test.setTimeout(180_000);
  let url = PERF_EVENT_URL;
  if (!url) {
    await virtualAuthenticator(page, context);
    const stamp = Date.now();
    await signUpWithPasskey(page, "Pia", `e2e-perf-${stamp}@example.com`);
    await page.goto("/organizers/new");
    await page.getByLabel("Organizer name").fill(`Pia Perf ${stamp}`);
    await page.getByRole("button", { name: "Create organizer" }).click();
    const slug = `pia-perf-${stamp}`;
    await expect(page.getByText(`Pia Perf ${stamp}`)).toBeVisible();
    await page.goto(`/o/${slug}/payments`);
    await page.getByRole("button", { name: "Create a Stripe account" }).click();
    await expect(page.getByText("You're ready to take payments.")).toBeVisible();
    await createSellableEvent(page, slug);
    url = `http://localhost:3003/${slug}/spring-social`;
  }

  // Turnstile waits for the buyer to start on the ticket form; nothing here touches it.
  const cloudflare: string[] = [];
  const scripts: string[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.hostname === "challenges.cloudflare.com") cloudflare.push(r.url());
    if (r.resourceType() === "script") scripts.push(u.pathname);
  });
  const js = await scriptBytes(page, url);
  // Both figures, so the ledger shows the full weight too (D7).
  console.log("js-bytes", js);
  test.info().annotations.push({ type: "js-bytes", description: JSON.stringify(js) });
  expect(cloudflare).toEqual([]);
  // The loader itself was weighed, so the budget can't pass on an empty page.
  expect(scripts).toContain("/assets/turnstile.js");
  expect(js.topFrame).toBeGreaterThan(0);
  expect(js.topFrame).toBeLessThanOrEqual(15 * 1024);

  const scores = await lighthouseScores(url, PERF_EVENT_URL ? 3 : 1);
  console.log("lighthouse", scores);
  test.info().annotations.push({ type: "lighthouse", description: JSON.stringify(scores) });
  if (PERF_EVENT_URL) expect(scores.performance).toBe(1);
  else expect(scores.performance).toBeGreaterThanOrEqual(0.95);
  expect(scores.accessibility).toBe(1);
  expect(scores.bestPractices).toBe(1);
});
