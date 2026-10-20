import { readFileSync, rmSync } from "node:fs";
import { type Browser, devices, expect, test } from "@playwright/test";
import {
  API,
  cameraShowing,
  createSellableEvent,
  signUpWithPasskey,
  virtualAuthenticator,
} from "./helpers";

const SHOP = "http://localhost:3003";

test("an organizer connects Stripe, a buyer pays on a phone, and the order shows up", async ({
  page,
  context,
  browser,
}) => {
  // Three camera scans at the door ride on top of the sale.
  test.slow();
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
  // The event page shows the one all-in price: $25 plus the $1.00 fee.
  await expect(shop.getByText("$26.00")).toBeVisible();
  await expect(shop.getByText("Includes $1.00 service fee")).toBeVisible();
  await shop.getByLabel("General").selectOption("2");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await expect(shop).toHaveURL(/\/checkout\//);
  // 2 × $26.00, 10% off the $25 price, no tax; the fee inside is 2 × (2% of $22.50 + $0.50) = $1.90.
  await expect(shop.getByText("$52.00")).toBeVisible();
  await expect(shop.getByText("−$5.10")).toBeVisible();
  await expect(shop.getByText("$46.90")).toBeVisible();
  await expect(shop.getByText("Includes $1.90 service fee")).toBeVisible();
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

  // The order page shows both codes, hands over the same PDF, and can send the email again.
  await expect(shop.locator("img.qr")).toHaveCount(2);
  await expect(shop.locator("img.qr").first()).toBeVisible();
  await expect(shop.locator("img.qr").last()).toBeVisible();
  // Ada's code, as a phone held up to the door would show it.
  // Chrome's fake camera on Linux plays a JPEG with an odd side as a black 2×2 frame,
  // so the picture is clipped to even CSS pixels at one pixel each.
  const ada = shop.locator("img.qr").first();
  // A half-loaded image would hand the camera a blank or partial code.
  await expect(ada).toHaveJSProperty("complete", true);
  expect(await ada.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);
  await ada.scrollIntoViewIfNeeded();
  const box = await ada.boundingBox();
  if (!box) throw new Error("Ada's QR code has no box");
  const even = (n: number) => Math.ceil(n / 2) * 2;
  const qr = await shop.screenshot({
    type: "jpeg",
    quality: 95,
    scale: "css",
    clip: {
      x: Math.floor(box.x),
      y: Math.floor(box.y),
      width: even(box.x + box.width - Math.floor(box.x)),
      height: even(box.y + box.height - Math.floor(box.y)),
    },
  });
  const download = shop.waitForEvent("download");
  await shop.getByRole("link", { name: "Download all tickets (PDF)" }).click();
  expect((await download).suggestedFilename()).toBe(`tickets-${code}.pdf`);
  await shop.getByRole("button", { name: "Email me my tickets again" }).click();
  // Straight after the sale, the first email is either still going or less than 10 minutes old.
  await expect(
    shop
      .getByText("We're still trying to send the latest one.")
      .or(shop.getByText("We tried sending these less than 10 minutes ago.")),
  ).toBeVisible();

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

  // The owner downloads the orders as a spreadsheet.
  await page.getByRole("link", { name: "Report" }).click();
  const csv = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download orders (CSV)" }).click();
  const header = readFileSync(await (await csv).path(), "utf8").split(/\r?\n/)[0];
  expect(header).toContain("Tickets bought");
  expect(header).toContain("Fee paid by");

  // At the door, a phone's camera sees Ada's ticket: green, then amber when it's shown again.
  // The picture is a ticket credential: it goes once the scans are done, never into the report.
  const qrFile = test.info().outputPath("qr.mjpeg");
  const staff = {
    ...devices["Pixel 7"],
    permissions: ["camera"],
    baseURL: test.info().project.use.baseURL,
  };
  const cookies = await context.cookies();
  const scanAt = async (noNativeReader: boolean) => {
    if (!door) throw new Error("No door camera");
    const phone = await door.newContext(staff);
    await phone.addCookies(cookies);
    const p = await phone.newPage();
    // Without the browser's own reader, the scanner falls back to our zxing-wasm under the CSP.
    if (noNativeReader)
      await p.addInitScript(() => {
        delete (globalThis as { BarcodeDetector?: unknown }).BarcodeDetector;
      });
    await p.goto(`/o/${slug}`);
    await p.getByRole("link", { name: "Spring Social" }).click();
    await p.getByRole("link", { name: "Check-in" }).click();
    await p.getByRole("button", { name: "Start scanning" }).click();
    return { phone, p };
  };
  let door: Browser | undefined;
  try {
    door = await cameraShowing(qr, qrFile);
    const { phone, p } = await scanAt(false);
    // Green lasts 2.5 s, so its heading and Ada's name are checked together in one look.
    await expect(p.getByRole("region", { name: "Welcome in" })).toContainText("Ada · General", {
      timeout: 10_000,
    });
    // The frame keeps playing: green clears at 2.5 s, the repeat guard lets go at 3 s.
    await expect(p.getByRole("heading", { name: "Already checked in" })).toBeVisible({
      timeout: 10_000,
    });
    await expect(p.getByText("1 of 3 checked in")).toBeVisible();
    await phone.close();

    const fallback = await scanAt(true);
    expect(await fallback.p.evaluate(() => "BarcodeDetector" in globalThis)).toBe(false);
    await expect(fallback.p.getByRole("heading", { name: "Already checked in" })).toBeVisible({
      timeout: 10_000,
    });
    await expect(fallback.p.getByText("1 of 3 checked in")).toBeVisible();
  } finally {
    await door?.close();
    rmSync(qrFile, { force: true });
  }
});
