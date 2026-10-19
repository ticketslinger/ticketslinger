import { devices, expect, test } from "@playwright/test";
import { API, createSellableEvent, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const SHOP = "http://localhost:3003";

test("an organizer connects Stripe, a buyer pays on a phone, and the order shows up", async ({
  page,
  context,
  browser,
}) => {
  await virtualAuthenticator(page, context);
  const stamp = Date.now();
  await signUpWithPasskey(page, "Olive", `e2e-sell-${stamp}@example.com`);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Olive Live ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `olive-live-${stamp}`;
  await expect(page.getByText(`Olive Live ${stamp}`)).toBeVisible();

  // Create a Stripe account: the test-mode provider sends us straight back.
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();

  // A sellable event: General $25 (capacity 50), Volunteer free, code FRIENDS 10%, a required question.
  await createSellableEvent(page, slug);

  // The buyer has no account, and is on a phone (the config's Pixel 7).
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  // Every request to the shop, with the page it came from: token pages must send no Referer.
  const sent: Promise<[string, string | null]>[] = [];
  shop.on("request", (r) => {
    if (r.url().startsWith(SHOP)) sent.push(r.headerValue("referer").then((v) => [r.url(), v]));
  });
  await shop.goto(`${SHOP}/${slug}/spring-social?code=FRIENDS`);
  await shop.getByLabel("General").selectOption("2");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await expect(shop).toHaveURL(/\/checkout\//);
  // 2 × $25, 10% off, no tax, service fee 2 × (2% of $22.50 + $0.50) = $1.90.
  await expect(shop.getByText("Service fee")).toBeVisible();
  await expect(shop.getByText("$46.90")).toBeVisible();
  await shop.getByLabel("Email").fill(`buyer-${stamp}@example.com`);
  await shop.getByLabel("Name on ticket 1").fill("Ada");
  await shop.getByLabel("Name on ticket 2").fill("Grace");
  await shop.getByLabel("Dietary needs").fill("None");
  await shop.getByRole("button", { name: "Continue to payment" }).click();
  await expect(shop.getByText("Test mode: no card needed.")).toBeVisible();

  // What Stripe would send once the card goes through.
  expect((await fetch(`${API}/__test/stripe/pay-latest`, { method: "POST" })).status).toBe(200);
  await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 15_000 });
  await expect(shop.getByText("Ada", { exact: true })).toBeVisible();
  await expect(shop.getByText("Grace", { exact: true })).toBeVisible();
  // The checkout and order pages loaded our own script and stylesheet, and polled the
  // checkout, without the token riding along in a Referer.
  const requests = await Promise.all(sent);
  expect(requests.some(([url]) => url.endsWith("/assets/storefront.css"))).toBe(true);
  expect(requests.filter(([, ref]) => ref && /\/checkout\/|\/orders\//.test(ref))).toEqual([]);

  // The same link arrives by email, once the worker has sent it.
  const buyerMail = `${API}/__test/last-email?to=${encodeURIComponent(`buyer-${stamp}@example.com`)}`;
  await expect
    .poll(async () => (await fetch(buyerMail)).text(), { timeout: 15_000 })
    .toBe(shop.url());
  // It carries both QR codes inline and a two-page PDF of them.
  const code = /\/orders\/(TS-[0-9A-Z]{5})/.exec(shop.url())?.[1];
  const summary = await fetch(
    `${API}/__test/last-email-summary?to=${encodeURIComponent(`buyer-${stamp}@example.com`)}`,
  );
  expect(await summary.json()).toMatchObject({
    inline: 2,
    attachments: [`tickets-${code}.pdf`],
    pdf_pages: 2,
  });

  // A free ticket needs no payment at all.
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByLabel("Volunteer").selectOption("1");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await shop.getByLabel("Email").fill(`helper-${stamp}@example.com`);
  await shop.getByLabel("Name on ticket 1").fill("Lin");
  await shop.getByLabel("Dietary needs").fill("Vegan");
  await shop.getByRole("button", { name: "Get my free ticket" }).click();
  await expect(shop).toHaveURL(/\/orders\/TS-/);
  await expect(shop.getByText("Lin", { exact: true })).toBeVisible();
  await buyer.close();

  // The organizer sees both.
  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Spring Social" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await expect(page.getByText("2 orders · 3 tickets · $46.90 in sales")).toBeVisible();
});
