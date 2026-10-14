import { type BrowserContext, expect, type Page } from "@playwright/test";

export const API = process.env.E2E_API ?? "http://127.0.0.1:3001";
export const PASSWORD = "a long test passphrase";

/** A passkey that approves itself, so "Add a passkey" and "Sign in with a passkey" work headless. */
export async function virtualAuthenticator(page: Page, context: BrowserContext) {
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
    },
  });
}

/** Signs up and follows the verification link from the captured email. */
export async function signUp(page: Page, name: string, email: string) {
  await page.goto("/signup");
  await page.getByLabel("Name").fill(name);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(PASSWORD);
  // The Turnstile widget fills its hidden input once the check passes.
  await expect(page.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByText("Check your inbox")).toBeVisible();
  const mail = await fetch(`${API}/__test/last-email?to=${encodeURIComponent(email)}`);
  expect(mail.status).toBe(200);
  await page.goto(await mail.text());
  await expect(page.getByText("Your email is verified")).toBeVisible();
}

export async function addPasskey(page: Page) {
  await page.goto("/security");
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Add a passkey" }).click();
  await expect(page.getByText("Passkey added")).toBeVisible();
}

/** Writing needs a second factor: add a passkey, then sign in with it. */
export async function signUpWithPasskey(page: Page, name: string, email: string) {
  await signUp(page, name, email);
  await addPasskey(page);
  await page.goto("/");
  await page.getByRole("button", { name: "Sign out" }).click();
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(/\/$/);
}

/**
 * Builds and publishes "Spring Social" a month from now: General $25 and Volunteer free in one
 * capacity of 50, the code FRIENDS for 10% off, and a required "Dietary needs" question asked
 * once per order. The organizer must already take payments.
 */
export async function createSellableEvent(page: Page, slug: string) {
  const day = new Date(Date.now() + 30 * 86_400_000).toISOString().slice(0, 10);
  const size = page.viewportSize();
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`/o/${slug}/events/new`);
  await page.getByLabel("Event name").fill("Spring Social");
  await page.getByLabel("Time zone").selectOption("America/Denver");
  await page.getByLabel("Starts").fill(`${day}T19:00`);
  await page.getByLabel("Ends").fill(`${day}T23:00`);
  await page.getByRole("button", { name: "Create event" }).click();
  await expect(page.getByRole("heading", { name: "Spring Social" })).toBeVisible();

  await page.getByRole("link", { name: "Tickets" }).click();
  for (const [name, price] of [
    ["General", "25.00"],
    ["Volunteer", "0.00"],
  ] as const) {
    await page.getByRole("button", { name: "Add a ticket" }).click();
    await page.getByLabel("Name").fill(name);
    await page.getByLabel("Price (USD)").fill(price);
    await page.getByRole("button", { name: "Add ticket" }).click();
    await expect(page.getByRole("cell", { name, exact: true })).toBeVisible();
  }

  await page.getByRole("button", { name: "Add a capacity" }).click();
  await page.getByLabel("Name").fill("Venue");
  await page.getByLabel("How many tickets").fill("50");
  await page.getByRole("checkbox", { name: "General" }).check();
  await page.getByRole("checkbox", { name: "Volunteer" }).check();
  await page.getByRole("button", { name: "Add capacity" }).click();
  await expect(page.getByRole("cell", { name: "50", exact: true })).toBeVisible();

  await page.getByRole("link", { name: "Promo codes" }).click();
  await page.getByRole("button", { name: "Add a promo code" }).click();
  await page.getByLabel("Code", { exact: true }).fill("friends");
  await page.getByLabel("Percent off").fill("10");
  await page.getByRole("button", { name: "Add promo code" }).click();
  await expect(page.getByRole("row", { name: /FRIENDS 10% off/ })).toBeVisible();

  await page.getByRole("link", { name: "Questions" }).click();
  await page.getByRole("button", { name: "Add a question" }).click();
  await page.getByLabel("Question", { exact: true }).fill("Dietary needs");
  await page.getByLabel("Answer with").selectOption("text");
  await page.getByLabel("Buyers have to answer").check();
  await page.getByRole("button", { name: "Add question" }).click();
  await expect(page.getByRole("row", { name: /Dietary needs Required/ })).toBeVisible();

  await page.getByRole("button", { name: "Go live" }).click();
  await expect(page.getByRole("button", { name: "Back to draft" })).toBeVisible();
  if (size) await page.setViewportSize(size);
}
