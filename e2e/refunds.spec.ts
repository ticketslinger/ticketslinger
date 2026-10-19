import { devices, expect, test } from "@playwright/test";
import { API, createSellableEvent, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const SHOP = "http://localhost:3003";

// What Stripe would send once the worker has asked it for the newest refund.
const settleRefund = () =>
  expect
    .poll(
      async () => (await fetch(`${API}/__test/stripe/refund-latest`, { method: "POST" })).status,
      {
        timeout: 15_000,
      },
    )
    .toBe(200);

test("an organizer refunds one ticket, then cancels the event", async ({
  page,
  context,
  browser,
}) => {
  await virtualAuthenticator(page, context);
  const stamp = Date.now();
  await signUpWithPasskey(page, "Rue", `e2e-refund-${stamp}@example.com`);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Rue Events ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `rue-events-${stamp}`;
  await expect(page.getByText(`Rue Events ${stamp}`)).toBeVisible();
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  await createSellableEvent(page, slug);

  // The buyer takes 2 × $25 with no code: $50.00 plus a $2.00 service fee.
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByLabel("General").selectOption("2");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  const email = `refund-buyer-${stamp}@example.com`;
  await shop.getByLabel("Email").fill(email);
  await shop.getByLabel("Name on ticket 1").fill("Ada");
  await shop.getByLabel("Name on ticket 2").fill("Grace");
  await shop.getByLabel("Dietary needs").fill("None");
  await shop.getByRole("button", { name: "Continue to payment" }).click();
  await expect(shop.getByText("Test mode: no card needed.")).toBeVisible();
  expect((await fetch(`${API}/__test/stripe/pay-latest`, { method: "POST" })).status).toBe(200);
  await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 15_000 });

  // Refund ticket 1. Our fee stays unless the organizer ticks the box.
  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Spring Social" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await page.getByRole("link", { name: /^TS-/ }).click();
  await page.getByLabel("General, Ada").check();
  await page.getByRole("button", { name: "Refund 1 ticket · $25.00" }).click();
  await expect(
    page.getByText(
      "Refund $25.00 to the card they paid with? Their tickets stop working straight away. This can't be undone.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Yes, refund" }).click();
  const ada = page.getByRole("listitem").filter({ hasText: "General, Ada" });
  await expect(ada).toContainText("Refund on its way");
  await settleRefund();
  await page.reload();
  await expect(ada).toContainText("Refunded");

  await page.getByRole("link", { name: "Spring Social" }).click();
  await expect(
    page.getByText("Refunded or on its way $25.00 · Our fee $2.00 · Yours $25.00"),
  ).toBeVisible();

  // The buyer's page, reached from the email link, agrees: ticket 1 is out, ticket 2 isn't.
  const mail = `${API}/__test/last-email?to=${encodeURIComponent(email)}`;
  await expect.poll(async () => (await fetch(mail)).ok, { timeout: 15_000 }).toBe(true);
  await shop.goto(await (await fetch(mail)).text());
  await expect(
    shop.getByText("Part of this order was refunded: $25.00 is on its way back to your card."),
  ).toBeVisible();
  const tickets = shop.locator(".order-tickets li");
  await expect(tickets.filter({ hasText: "Ada" })).toContainText("Refunded");
  await expect(tickets.filter({ hasText: "Grace" })).not.toContainText("Refunded");

  // Lost the email? Ask for the link again: the new one opens the order, the old one doesn't.
  const old = shop.url();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByRole("link", { name: "Already bought? Get your ticket link again" }).click();
  await shop.getByLabel("Email").fill(email);
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Send my link" }).click();
  await expect(
    shop.getByText(
      "Done. If that address bought tickets for Spring Social, the link is on its way.",
    ),
  ).toBeVisible();
  await expect.poll(async () => (await fetch(mail)).text(), { timeout: 15_000 }).not.toBe(old);
  await shop.goto(await (await fetch(mail)).text());
  await expect(tickets.filter({ hasText: "Grace" })).toBeVisible();
  await shop.goto(old);
  await expect(shop.getByText("That link doesn't open an order.")).toBeVisible();
  await buyer.close();

  // Cancel the event: the name has to match, and the fee goes back too.
  await page.getByRole("link", { name: "Details" }).click();
  await page.getByRole("button", { name: "Cancel this event" }).click();
  const go = page.getByRole("button", { name: "Cancel the event and refund everyone" });
  await page.getByLabel("Type the event's name to confirm").fill("Spring Soc");
  await expect(go).toBeDisabled();
  await page.getByLabel("Type the event's name to confirm").fill("Spring Social");
  await go.click();
  await expect(page.getByText("Cancelled. We're refunding 1 order, $26.00 in all.")).toBeVisible();

  await settleRefund();
  await page.getByRole("link", { name: "Orders" }).click();
  await expect(page.getByText("Refunded", { exact: true }).first()).toBeVisible();
});
