import { type Browser, devices, expect, type Page, test } from "@playwright/test";
import {
  API,
  createSellableEvent,
  grantStaff,
  ownerSql,
  signUpWithPasskey,
  virtualAuthenticator,
} from "./helpers";

const SHOP = "http://localhost:3003";
const REASON = "Checking a report from a buyer";

/** A signed-in browser of its own, with its own passkey. */
async function person(browser: Browser, name: string, email: string) {
  const context = await browser.newContext({
    ...devices["Pixel 7"],
    baseURL: "http://localhost:3002",
  });
  const page = await context.newPage();
  await virtualAuthenticator(page, context);
  await signUpWithPasskey(page, name, email);
  return page;
}

/** An organizer who takes payments, owned by a fresh person. */
async function organizer(browser: Browser, stamp: number) {
  const page = await person(browser, "Rue", `e2e-owner-${stamp}@example.com`);
  const name = `Rue Events ${stamp}`;
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(name);
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText(name)).toBeVisible();
  const slug = `rue-events-${stamp}`;
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  return { page, name, slug };
}

/** A buyer takes 2 x $25 on the fake payment path: $50.00 plus a $2.00 fee. */
async function buy(browser: Browser, slug: string, stamp: number) {
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByLabel("General").selectOption("2");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await shop.getByLabel("Email").fill(`staff-buyer-${stamp}@example.com`);
  await shop.getByLabel("Name on ticket 1").fill("Ada");
  await shop.getByLabel("Name on ticket 2").fill("Grace");
  await shop.getByLabel("Dietary needs").fill("None");
  await shop.getByRole("button", { name: "Continue to payment" }).click();
  await expect(shop.getByText("Test mode: no card needed.")).toBeVisible();
  expect((await fetch(`${API}/__test/stripe/pay-latest`, { method: "POST" })).status).toBe(200);
  await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 15_000 });
}

/** Staff searches for the organizer and starts support mode with a passkey touch. */
async function startSupport(staff: Page, name: string, slug: string) {
  await staff.goto("/staff");
  await expect(staff.getByRole("heading", { name: "Support", exact: true })).toBeVisible();
  await staff.getByLabel("Find an organizer").fill(slug);
  await staff.getByRole("button", { name: "Search" }).click();
  await staff.getByLabel(name).check();
  await staff.getByLabel("Why do you need to look?").fill(REASON);
  await staff.getByRole("button", { name: "Touch your passkey to start" }).click();
  await expect(
    staff.getByRole("status").filter({ hasText: `Support mode: viewing ${name}` }),
  ).toBeVisible();
}

/** Opens an action's dialog, shows the confirm button waits for a reason, then confirms. */
async function confirmAction(staff: Page, open: string, reason: string, typed?: string) {
  const panel = staff.locator("details.staff-actions");
  if (!(await panel.evaluate((d: HTMLDetailsElement) => d.open)))
    await panel.locator("summary").click();
  await staff.getByRole("button", { name: open }).first().click();
  const go = staff.getByRole("button", { name: "Touch your passkey to confirm" });
  if (typed !== undefined) {
    await staff.getByLabel("Type the event's name to confirm").fill(typed);
  }
  await staff.getByLabel("Reason").fill("too short");
  await expect(go).toBeDisabled();
  await staff.getByLabel("Reason").fill(reason);
  return go;
}

test("staff reads one organizer, read-only", async ({ browser }) => {
  test.setTimeout(120_000);
  const stamp = Date.now();
  const { page: owner, name, slug } = await organizer(browser, stamp);
  await createSellableEvent(owner, slug);
  await buy(browser, slug, stamp);

  const email = `e2e-staff-${stamp}@example.com`;
  const staff = await person(browser, "Sam Support", email);
  grantStaff(email);
  await startSupport(staff, name, slug);

  // The countdown moves: a minute on the device's clock takes the banner down by one.
  const banner = staff.getByRole("status").filter({ hasText: "Support mode:" });
  await staff.clock.install();
  await staff.reload();
  const left = async () => Number(/(\d+) min left/.exec((await banner.textContent()) ?? "")?.[1]);
  await expect(banner).toContainText("min left");
  const before = await left();
  await staff.clock.fastForward("01:00");
  await expect(banner).toContainText(`${before - 1} min left`);

  // An order opens, and nothing on the event can be saved.
  await staff.goto(`/o/${slug}`);
  await staff.getByRole("link", { name: "Spring Social" }).click();
  await expect(staff.getByRole("button", { name: "Save changes" })).toHaveCount(0);
  await staff.getByRole("link", { name: "Orders" }).click();
  await staff.getByRole("link", { name: /^TS-/ }).click();
  await expect(staff.getByRole("heading", { name: /^Order TS-/ })).toBeVisible();
  await expect(staff.getByRole("button", { name: "Save changes" })).toHaveCount(0);

  // End: back to the Support screen, banner gone.
  await banner.getByRole("button", { name: "End" }).click();
  await staff.getByRole("button", { name: "End support mode" }).click();
  await expect(staff).toHaveURL(/\/staff$/);
  await expect(banner).toHaveCount(0);

  // The owner's Audit page shows the visit.
  await owner.goto(`/o/${slug}/audit`);
  await expect(
    owner.getByText(`TicketSlinger support (Sam Support) viewed your organizer: "${REASON}"`),
  ).toBeVisible();
  await expect(
    owner.getByText("TicketSlinger support (Sam Support) ended support mode"),
  ).toBeVisible();
});

test("an hour that runs out sends staff back to the Support screen", async ({ browser }) => {
  test.setTimeout(120_000);
  const stamp = Date.now();
  const { name, slug } = await organizer(browser, stamp);
  const email = `e2e-staff-expiry-${stamp}@example.com`;
  const staff = await person(browser, "Sam Support", email);
  grantStaff(email);
  await startSupport(staff, name, slug);

  // The session runs out 20 seconds from now; the banner asks the server on its next half-minute tick.
  ownerSql(
    `UPDATE staff_sessions SET expires_at = now() + interval '20 seconds' WHERE organizer_slug = '${slug}' AND ended_at IS NULL`,
  );
  await staff.reload();
  await expect(staff.getByRole("status").filter({ hasText: "0 min left" })).toBeVisible();
  await expect(staff).toHaveURL(/\/staff$/, { timeout: 60_000 });
  await expect(staff.getByRole("status").filter({ hasText: "Support mode:" })).toHaveCount(0);
});

test("staff takes an organizer down and restores it", async ({ browser }) => {
  test.setTimeout(120_000);
  const stamp = Date.now();
  const { page: owner, name, slug } = await organizer(browser, stamp);
  await createSellableEvent(owner, slug);
  await buy(browser, slug, stamp);
  const email = `e2e-staff-takedown-${stamp}@example.com`;
  const staff = await person(browser, "Sam Support", email);
  grantStaff(email);
  await startSupport(staff, name, slug);

  // Suspend: the button waits for a reason of 10 characters.
  await (await confirmAction(staff, "Suspend organizer", REASON)).click();
  await expect(staff.getByRole("button", { name: "Lift suspension" })).toBeVisible();

  // The event page is gone for buyers, with the spec's words.
  const res = await fetch(`${SHOP}/${slug}/spring-social`);
  expect(res.status).toBe(410);
  const shop = await (await browser.newContext()).newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await expect(shop.getByRole("heading", { name: "This event isn't available." })).toBeVisible();

  // The owner sees the banner and can't save anything.
  await owner.goto(`/o/${slug}`);
  await expect(
    owner.getByRole("alert").filter({ hasText: `We've suspended ${name}.` }),
  ).toBeVisible();
  await owner.getByRole("link", { name: "Spring Social" }).click();
  await expect(owner.getByRole("heading", { name: "Spring Social" })).toBeVisible();
  await expect(owner.getByRole("button", { name: "Save changes" })).toHaveCount(0);

  // Refund: the event's name has to be typed out, and the totals come back.
  const go = await confirmAction(staff, "Refund every order", REASON, "Spring Soci");
  await expect(go).toBeDisabled();
  await staff.getByLabel("Type the event's name to confirm").fill("Spring Social");
  await go.click();
  await expect(
    staff.getByText(
      "Refunded 1 order, $52.00 in all, $2.00 of it our fee. 0 unfinished payments stopped.",
    ),
  ).toBeVisible();

  // The order shows refunded once Stripe's word comes back.
  await expect
    .poll(
      async () => (await fetch(`${API}/__test/stripe/refund-latest`, { method: "POST" })).status,
      {
        timeout: 15_000,
      },
    )
    .toBe(200);
  await staff.goto(`/o/${slug}`);
  await staff.getByRole("link", { name: "Spring Social" }).click();
  await staff.getByRole("link", { name: "Orders" }).click();
  await expect(staff.getByRole("row", { name: /^TS-/ })).toContainText("Refunded");

  // Lift: the event sells again.
  await (await confirmAction(staff, "Lift suspension", REASON)).click();
  await expect(staff.getByRole("button", { name: "Suspend organizer" })).toBeVisible();
  await shop.reload();
  await expect(shop.getByRole("button", { name: "Get tickets" })).toBeVisible();
});
