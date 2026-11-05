import { type Browser, devices, expect, type Page, test } from "@playwright/test";
import { API, createSellableEvent, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const SHOP = "http://localhost:3003";

/** An organizer with a live event, and a buyer who has paid for two tickets and fixed ticket 2's name. */
async function sale(
  page: Page,
  context: Parameters<typeof virtualAuthenticator>[1],
  browser: Browser,
) {
  await virtualAuthenticator(page, context);
  const stamp = Date.now();
  await signUpWithPasskey(page, "Olive", `e2e-fixes-${stamp}@example.com`);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Olive Fixes ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `olive-fixes-${stamp}`;
  await expect(page.getByText(`Olive Fixes ${stamp}`)).toBeVisible();
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  await createSellableEvent(page, slug);

  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social?code=FRIENDS`);
  // One price: $25 plus the $1.00 fee, with no "added at checkout" warning.
  await expect(shop.getByText("$26.00")).toBeVisible();
  await expect(shop.getByText("Includes $1.00 service fee")).toBeVisible();
  await expect(shop.getByText("A service fee is added")).toHaveCount(0);
  await shop.getByLabel("General").selectOption("2");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await expect(shop).toHaveURL(/\/checkout\//);
  await expect(shop.getByText("$46.90")).toBeVisible();
  await expect(shop.getByText("Includes $1.90 service fee")).toBeVisible();
  await shop.getByLabel("Email").fill(`buyer-${stamp}@example.com`);
  await shop.getByLabel("Name on ticket 1").fill("Ada");
  await shop.getByLabel("Name on ticket 2").fill("Grace");
  await shop.getByLabel("Dietary needs").fill("None");
  await shop.getByRole("button", { name: "Continue to payment" }).click();
  // The fake provider has no card form, so no "Pay $46.90" button: this line is what it shows.
  await expect(shop.getByText("Test mode: no card needed.")).toBeVisible();
  expect((await fetch(`${API}/__test/stripe/pay-latest`, { method: "POST" })).status).toBe(200);
  await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 15_000 });

  // Grace is spelled wrong; the buyer fixes it from the order page.
  await shop.getByLabel("Name on ticket 2").fill("Grace Hopper");
  await shop.getByRole("button", { name: "Save name" }).nth(1).click();
  await expect(shop.getByText("Saved. Ticket 2 now says Grace Hopper.")).toBeVisible();
  await expect(shop.getByText("Grace Hopper", { exact: true })).toBeVisible();
  return { slug, shop, buyer };
}

test("a buyer sees one price, pays it, and fixes a name", async ({ page, context, browser }) => {
  test.slow();
  const { shop, buyer } = await sale(page, context, browser);
  const download = shop.waitForEvent("download");
  await shop.getByRole("link", { name: "Download all tickets (PDF)" }).click();
  expect((await download).suggestedFilename()).toMatch(/^tickets-TS-[0-9A-Z]{5}\.pdf$/);
  await buyer.close();
});

test("the organizer renames a ticket and the audit log shows both changes", async ({
  page,
  context,
  browser,
}) => {
  // The names lock needs a real minute to pass.
  test.setTimeout(180_000);
  const { slug, shop, buyer } = await sale(page, context, browser);
  await page.setViewportSize({ width: 1280, height: 900 });

  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Spring Social" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await page.getByRole("link", { name: /^TS-/ }).click();
  await page
    .getByRole("listitem")
    .filter({ hasText: "Grace Hopper" })
    .getByRole("button", { name: "Edit name" })
    .click();
  await page.getByLabel("General, name on ticket").fill("Grace B. Hopper");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("Grace B. Hopper")).toBeVisible();

  await page.goto(`/o/${slug}/audit`);
  await expect(
    page.getByText(`The buyer changed a ticket's name from "Grace" to "Grace Hopper"`, {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText(`Changed a ticket's name from "Grace Hopper" to "Grace B. Hopper"`, {
      exact: true,
    }),
  ).toBeVisible();

  // The lock can't be set after the event starts (it starts at 19:00 Denver time, a month out).
  const day = new Date(Date.now() + 30 * 86_400_000).toISOString().slice(0, 10);
  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Spring Social" }).click();
  const lock = page.getByLabel("Buyers can change names until");
  await lock.fill(`${day}T20:00`);
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Pick a time before the event starts.")).toBeVisible();

  // The next whole minute, at least 15 s away (the field has minute precision).
  let at = Math.ceil(Date.now() / 60_000) * 60_000;
  if (at - Date.now() < 15_000) at += 60_000;
  const local = new Date(at).toLocaleString("sv-SE", { timeZone: "America/Denver" });
  await lock.fill(`${local.slice(0, 10)}T${local.slice(11, 16)}`);
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Saved.", { exact: true })).toBeVisible();

  // Once the minute is up, the order page has no name forms left.
  await expect
    .poll(
      async () => {
        await shop.reload();
        return shop.getByLabel(/^Name on ticket/).count();
      },
      { timeout: 90_000, intervals: [3_000] },
    )
    .toBe(0);
  await buyer.close();
});
