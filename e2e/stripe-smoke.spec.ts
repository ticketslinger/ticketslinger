// The manual stripe-smoke workflow: real test-mode Stripe, a connected account
// that already takes charges, and webhooks through `stripe listen`. It skips
// itself everywhere else.
import { execFileSync } from "node:child_process";
import { devices, expect, type Frame, type Page, test } from "@playwright/test";
import { signUpWithPasskey, virtualAuthenticator } from "./helpers";
import { scriptBytes } from "./perf";

const SHOP = "http://localhost:3003";
const ACCOUNT = process.env.STRIPE_SMOKE_ACCOUNT ?? "";
// The run's own throwaway database, which start.ts creates fresh, as its owner.
const DB = `postgres://ts_owner:ts_owner@${process.env.E2E_PG ?? "localhost:54329"}/ticketslinger_e2e?sslmode=disable`;

/** Runs one statement with psql; values go in as psql variables, quoted by psql. */
function sql(query: string, vars: Record<string, string>): string {
  const args = [DB, "-XtA", "-v", "ON_ERROR_STOP=1"];
  for (const [k, v] of Object.entries(vars)) args.push("-v", `${k}=${v}`);
  return execFileSync("psql", args, { input: query, encoding: "utf8" }).trim();
}

/** GET from Stripe with the run's test key, which never reaches a log. */
async function stripe(path: string, account?: string) {
  const r = await fetch(`https://api.stripe.com/v1/${path}`, {
    headers: {
      Authorization: `Bearer ${process.env.TS_STRIPE_SECRET_KEY}`,
      ...(account ? { "Stripe-Account": account } : {}),
    },
  });
  expect(r.status, `GET ${path.split("?")[0]}`).toBe(200);
  return r.json();
}

test.skip(!ACCOUNT, "manual stripe-smoke workflow only");

/**
 * The frame holding the Payment Element's card form. Stripe mounts several
 * iframes with the same title, so pick the one that actually has the Card tab
 * or the card number field, rather than guessing at Stripe's frame names.
 */
async function cardFrame(shop: Page): Promise<Frame> {
  let found: Frame | undefined;
  await expect
    .poll(
      async () => {
        for (const f of shop.frames()) {
          const hit = f
            .getByRole("button", { name: "Card", exact: true })
            .or(f.locator('[name="number"]'));
          if (await hit.count().catch(() => 0)) {
            found = f;
            return true;
          }
        }
        return false;
      },
      { timeout: 15_000, message: "Stripe's card form never showed" },
    )
    .toBe(true);
  return found as Frame;
}

/** The Payment Element's card form. A mouse click can land on a resizing iframe, so Pay gets a DOM click. */
async function payByCard(shop: Page, number: string) {
  const frame = await cardFrame(shop);
  const card = frame.getByRole("button", { name: "Card", exact: true });
  const cardNumber = frame.locator('[name="number"]');
  await expect(card.or(cardNumber).first()).toBeVisible({ timeout: 15_000 });
  if (await card.isVisible()) await card.click();
  await cardNumber.fill(number);
  await frame.locator('[name="expiry"]').fill("12 / 34");
  await frame.locator('[name="cvc"]').fill("123");
  // Rendered with the other card fields, so no wait is needed once they're filled.
  const zip = frame.locator('[name="postalCode"]');
  if (await zip.isVisible().catch(() => false)) await zip.fill("42424");
  await shop.getByRole("button", { name: /^Pay \$/ }).dispatchEvent("click");
}

/** Stripe's 3D Secure test page, wherever in the frame tree it lands: approve it. */
async function complete3DS(shop: Page) {
  await expect
    .poll(
      async () => {
        for (const f of shop.frames()) {
          const ok = f
            .locator(
              '#test-source-authorize-3ds, button:text-is("Complete"), button:text-is("Complete authentication")',
            )
            .first();
          if (await ok.count().catch(() => 0)) {
            await ok.dispatchEvent("click");
            return true;
          }
        }
        return false;
      },
      { timeout: 60_000, message: "Stripe's 3D Secure test page never showed" },
    )
    .toBe(true);
}

test("real test-mode Stripe: a card, a 3D Secure card, a refund and a cancellation", async ({
  page,
  context,
  browser,
}) => {
  test.setTimeout(240_000);
  await virtualAuthenticator(page, context);
  const stamp = process.env.GITHUB_RUN_ID ?? `${Date.now()}`;
  await signUpWithPasskey(page, "Sam", `smoke-org+${stamp}@example.com`);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Smoke Shop ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `smoke-shop-${stamp}`;
  await expect(page.getByText(`Smoke Shop ${stamp}`)).toBeVisible();

  // "Smoke Social" a month out: General at $25, capacity 10.
  const day = new Date(Date.now() + 30 * 86_400_000).toISOString().slice(0, 10);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`/o/${slug}/events/new`);
  await page.getByLabel("Event name").fill("Smoke Social");
  await page.getByLabel("Time zone").selectOption("America/Denver");
  await page.getByLabel("Starts").fill(`${day}T19:00`);
  await page.getByLabel("Ends").fill(`${day}T23:00`);
  await page.getByRole("button", { name: "Create event" }).click();
  await expect(page.getByRole("heading", { name: "Smoke Social" })).toBeVisible();
  await page.getByRole("link", { name: "Tickets" }).click();
  await page.getByRole("button", { name: "Add a ticket" }).click();
  await page.getByLabel("Name").fill("General");
  await page.getByLabel("Price (USD)").fill("25.00");
  await page.getByRole("button", { name: "Add ticket" }).click();
  await expect(page.getByRole("cell", { name: "General", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Add a capacity" }).click();
  await page.getByLabel("Name").fill("Venue");
  await page.getByLabel("How many tickets").fill("10");
  await page.getByRole("checkbox", { name: "General" }).check();
  await page.getByRole("button", { name: "Add capacity" }).click();
  await expect(page.getByRole("cell", { name: "10", exact: true })).toBeVisible();

  // Hosted onboarding has a captcha, so the account that already takes charges is attached directly.
  const attached = sql(
    `update organizers set stripe_account_id = :'acct', stripe_link_method = 'created',
       stripe_connected_at = now(), stripe_charges_enabled = true, stripe_details_submitted = true
     where slug = :'slug' returning id`,
    { acct: ACCOUNT, slug },
  );
  expect(attached).not.toBe("");
  await page.getByRole("button", { name: "Go live" }).click();
  await expect(page.getByRole("button", { name: "Back to draft" })).toBeVisible();

  // Two buyers, 2 × General each: $50.00 plus a $2.00 service fee.
  const buy = async (who: string, number: string, threeDS: boolean, weigh = false) => {
    const buyer = await browser.newContext({ ...devices["Pixel 7"] });
    const shop = await buyer.newPage();
    await shop.goto(`${SHOP}/${slug}/smoke-social`);
    await shop.getByLabel("General").selectOption("2");
    await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
      timeout: 15_000,
    });
    await shop.getByRole("button", { name: "Get tickets" }).click();
    await shop.getByLabel("Email").fill(`smoke+${stamp}-${who}@example.com`);
    await shop.getByLabel("Name on ticket 1").fill(`${who} One`);
    await shop.getByLabel("Name on ticket 2").fill(`${who} Two`);
    await shop.getByRole("button", { name: "Continue to payment" }).click();
    const pay = shop.getByRole("button", { name: "Pay $52.00" });
    await expect(pay).toBeVisible({ timeout: 30_000 });
    if (weigh) {
      // The checkout's own JS, Stripe's loader included, weighed with the Payment Element up (D7).
      await cardFrame(shop);
      const js = await scriptBytes(shop, shop.url());
      console.log("js-bytes", js);
      test.info().annotations.push({ type: "js-bytes", description: JSON.stringify(js) });
      expect(js.topFrame).toBeGreaterThan(0);
      expect(js.topFrame).toBeLessThanOrEqual(60 * 1024);
      await expect(pay).toBeVisible({ timeout: 30_000 });
    }
    await payByCard(shop, number);
    if (threeDS) await complete3DS(shop);
    // Only the webhook, through stripe listen, makes the order.
    await expect(shop).toHaveURL(/\/orders\/TS-[0-9A-Z]{5}\?t=/, { timeout: 60_000 });
    const code = new URL(shop.url()).pathname.split("/").pop() ?? "";
    await buyer.close();
    return code;
  };
  const first = await buy("Ada", "4242 4242 4242 4242", false, true);
  // Stripe's test card that asks for 3D Secure on every payment.
  await buy("Bo", "4000 0027 6000 3184", true);

  // Refund ticket 1 of the first order, with our fee: $25.00 + $1.00.
  await page.goto(`/o/${slug}`);
  await page.getByRole("link", { name: "Smoke Social" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await page.getByRole("link", { name: first }).click();
  await page.getByLabel("General, Ada One").check();
  await page.getByLabel("Give back our service fee too").check();
  await page.getByRole("button", { name: "Refund 1 ticket · $26.00" }).click();
  await page.getByRole("button", { name: "Yes, refund" }).click();
  const ada = page.getByRole("listitem").filter({ hasText: "General, Ada One" });
  await expect(async () => {
    await page.reload();
    await expect(ada).toContainText("Refunded", { timeout: 2_000 });
  }).toPass({ timeout: 60_000 });

  // Cancel the event: everything left goes back, fee included.
  await page.getByRole("link", { name: "Smoke Social" }).click();
  await page.getByRole("link", { name: "Details" }).click();
  await page.getByRole("button", { name: "Cancel this event" }).click();
  await page.getByLabel("Type the event's name to confirm").fill("Smoke Social");
  await page.getByRole("button", { name: "Cancel the event and refund everyone" }).click();
  await expect(page.getByText("Cancelled. We're refunding 2 orders, $78.00 in all.")).toBeVisible();
  await page.getByRole("link", { name: "Orders" }).click();
  await expect(async () => {
    await page.reload();
    await expect(page.getByText("Refunded", { exact: true })).toHaveCount(2, { timeout: 2_000 });
  }).toPass({ timeout: 60_000 });

  // Stripe's own records for the first order: all $52.00 back, and our $2.00 fee with it.
  const pi = sql(
    `select p.payment_intent_id from payments p join orders o on o.id = p.order_id where o.code = :'code'`,
    { code: first },
  );
  await expect
    .poll(
      async () =>
        (await stripe(`payment_intents/${pi}?expand[]=latest_charge`, ACCOUNT)).latest_charge
          .amount_refunded,
      { timeout: 60_000 },
    )
    .toBe(5200);
  const charge = (await stripe(`payment_intents/${pi}?expand[]=latest_charge`, ACCOUNT))
    .latest_charge;
  await expect
    .poll(
      async () => (await stripe(`application_fees/${charge.application_fee}`)).amount_refunded,
      {
        timeout: 60_000,
      },
    )
    .toBe(200);
});
