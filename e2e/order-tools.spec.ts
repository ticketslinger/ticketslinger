import { type Browser, devices, expect, type Page, test } from "@playwright/test";
import { API, createSellableEvent, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const SHOP = "http://localhost:3003";

type Summary = { subject: string; text: string };
const summary = async (to: string): Promise<Summary | null> => {
  const r = await fetch(`${API}/__test/last-email-summary?to=${encodeURIComponent(to)}`);
  return r.ok ? r.json() : null;
};
const lastLink = async (to: string) =>
  (await fetch(`${API}/__test/last-email?to=${encodeURIComponent(to)}`)).text();

/** An owner signed in with a passkey, whose organizer takes payments and sells "Spring Social". */
async function organizer(
  page: Page,
  context: Parameters<typeof virtualAuthenticator>[1],
  who: string,
) {
  await virtualAuthenticator(page, context);
  const stamp = Date.now();
  const owner = `e2e-${who}-${stamp}@example.com`;
  await signUpWithPasskey(page, "Otto", owner);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Otto ${who} ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `otto-${who}-${stamp}`;
  await expect(page.getByText(`Otto ${who} ${stamp}`)).toBeVisible();
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  await createSellableEvent(page, slug);
  await page.setViewportSize({ width: 1280, height: 900 });
  return { slug, stamp, owner };
}

/** A buyer on their phone takes one General online and pays. */
async function buyOne(browser: Browser, slug: string, email: string) {
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByLabel("General").selectOption("1");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await shop.getByLabel("Email").fill(email);
  await shop.getByLabel("Name on ticket 1").fill("Ada");
  await shop.getByLabel("Dietary needs").fill("None");
  await shop.getByRole("button", { name: "Continue to payment" }).click();
  await expect(shop.getByText("Test mode: no card needed.")).toBeVisible();
  expect((await fetch(`${API}/__test/stripe/pay-latest`, { method: "POST" })).status).toBe(200);
  await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 15_000 });
  await buyer.close();
}

/** From the event's Orders tab to a filled Add order form, submitted. */
async function addOrder(
  page: Page,
  slug: string,
  f: { kind: "Comp: free" | "Manual: paid outside TicketSlinger"; email: string; general: string },
  fill: () => Promise<void> = async () => {},
) {
  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Spring Social" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await page.getByRole("link", { name: "Add order" }).click();
  await page.getByLabel(f.kind).check();
  await page.getByLabel("Buyer's email").fill(f.email);
  await page.getByLabel("General · $25.00").fill(f.general);
  await fill();
  await page.getByRole("button", { name: "Add order" }).click();
  await expect(page.getByRole("heading", { name: /^Order TS-[0-9A-Z]{5}$/ })).toBeVisible();
}

test("a comp guest, a cancel and an alert", async ({ page, context, browser }) => {
  test.slow();
  const { slug, stamp, owner } = await organizer(page, context, "comp");

  // The owner asks for an email about each online order, and gets one for the next sale.
  await page.goto(`/o/${slug}/team`);
  const alerts = page.getByLabel("Email me about each new online order");
  // Controlled by the saved setting: it ticks once the API has answered, so click, then wait.
  await expect(alerts).toBeEnabled();
  await alerts.click();
  await expect(alerts).toBeChecked();
  await page.reload();
  await expect(alerts).toBeChecked();
  await buyOne(browser, slug, `alert-buyer-${stamp}@example.com`);
  await expect
    .poll(async () => (await summary(owner))?.subject, { timeout: 15_000 })
    .toMatch(/^New order TS-/);

  // Two free tickets for the headliner's guest, with a note only the team sees.
  const guest = `guest-${stamp}@example.com`;
  await addOrder(page, slug, { kind: "Comp: free", email: guest, general: "2" }, async () => {
    await page.getByLabel("Note for your team (optional)").fill("Headliner's plus-one");
  });
  await expect(page.locator(".pill.source")).toHaveText("Comp");
  await expect(page.getByText("Headliner's plus-one")).toBeVisible();
  await page.getByLabel("Add a note").fill("Meet them at the side door");
  await page.getByRole("button", { name: "Add note" }).click();
  await expect(page.getByText("Meet them at the side door")).toBeVisible();

  // The guest's email and order page both say they paid nothing.
  await expect
    .poll(async () => (await summary(guest))?.text, { timeout: 15_000 })
    .toContain("Complimentary.");
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(await lastLink(guest));
  await expect(shop.getByText(/· Complimentary ·/)).toBeVisible();
  const tickets = shop.locator(".order-tickets li");
  await expect(tickets).toHaveCount(2);

  // One of the two was a mistake: cancel it, with nothing to refund.
  await page.getByText("More", { exact: true }).first().click();
  await page.getByRole("button", { name: "Cancel ticket" }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("They paid nothing, so there's nothing to refund.")).toBeVisible();
  await dialog.getByLabel("Reason (optional)").fill("Duplicate");
  await dialog.getByRole("button", { name: "Cancel ticket" }).click();
  await expect(dialog).toBeHidden();
  await expect(page.locator(".pill").filter({ hasText: "Cancelled" })).toHaveCount(1);

  await shop.reload();
  await expect(tickets.filter({ hasText: "Cancelled" })).toHaveCount(1);
  await buyer.close();
});

test("cancelling the event lists cash orders to pay back", async ({ page, context, browser }) => {
  test.slow();
  const { slug, stamp } = await organizer(page, context, "cash");

  const cash = `cash-${stamp}@example.com`;
  await addOrder(
    page,
    slug,
    { kind: "Manual: paid outside TicketSlinger", email: cash, general: "1" },
    async () => {
      await page.getByLabel("How they paid").selectOption("cash");
      await page.getByLabel("What they paid (USD)").fill("40.00");
    },
  );
  await expect(page.locator(".pill.source")).toHaveText("Manual · cash");
  const code = (await page.getByRole("heading", { name: /^Order TS-/ }).innerText()).slice(6);

  // The buyer's page says who they paid.
  await expect.poll(async () => (await summary(cash))?.subject, { timeout: 15_000 }).toBeTruthy();
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(await lastLink(cash));
  await expect(shop.getByText(/· Paid to the organizer \(\$40\.00\) ·/)).toBeVisible();
  await buyer.close();

  // The report counts it apart from Stripe money, while its ticket still stands.
  await page.getByRole("link", { name: "Spring Social" }).click();
  await page.getByRole("link", { name: "Report" }).click();
  await expect(page.locator("dl div").filter({ hasText: "Taken by hand" })).toContainText("$40.00");

  await page.getByRole("link", { name: "Details" }).click();
  await page.getByRole("button", { name: "Cancel this event" }).click();
  await page.getByLabel("Type the event's name to confirm").fill("Spring Social");
  await page.getByRole("button", { name: "Cancel the event and refund everyone" }).click();
  await expect(page.getByText("You took these by hand, so pay them back yourself:")).toBeVisible();
  await expect(page.getByRole("listitem").filter({ hasText: code })).toHaveText(`${code} · $40.00`);
  await expect
    .poll(async () => (await summary(cash))?.text, { timeout: 15_000 })
    .toContain("will be in touch about your $40.00");
});
