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
