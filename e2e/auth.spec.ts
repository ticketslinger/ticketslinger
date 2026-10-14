import { expect, test } from "@playwright/test";

const API = process.env.E2E_API ?? "http://127.0.0.1:3001";

test("sign up, verify, add a passkey, sign out, sign back in with the passkey, create an organizer", async ({
  page,
  context,
}) => {
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

  const email = `e2e-${Date.now()}@example.com`;
  await page.goto("/signup");
  await page.getByLabel("Name").fill("Eve");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("a long test passphrase");
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByText("Check your inbox")).toBeVisible();

  const mail = await fetch(`${API}/__test/last-email?to=${encodeURIComponent(email)}`);
  expect(mail.status).toBe(200);
  await page.goto(await mail.text());
  await expect(page.getByText("Your email is verified")).toBeVisible();

  // A password-only session has to confirm the password to add a passkey.
  await page.goto("/security");
  await page.getByLabel("Password").fill("a long test passphrase");
  await page.getByRole("button", { name: "Add a passkey" }).click();
  await expect(page.getByText("Passkey added")).toBeVisible();

  await page.goto("/");
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/signin$/);

  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(/\/$/);

  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Eve's Events");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Eve's Events")).toBeVisible();

  // The audit log is owner-only and needs a second-factor session, so this
  // passes only because the passkey sign-in counted as one.
  const audit = await page.evaluate(async () => {
    const res = await fetch("/api/organizers/eve-s-events/audit");
    return { status: res.status, body: await res.json() };
  });
  expect(audit.status).toBe(200);
  expect(audit.body).toContainEqual(expect.objectContaining({ action: "organizer.created" }));
});
