import { expect, type Page, test } from "@playwright/test";
import { addPasskey, signUp, virtualAuthenticator } from "./helpers";

test("sign up, verify, create an organizer, then reach its audit log only after a passkey sign-in", async ({
  page,
  context,
}) => {
  await virtualAuthenticator(page, context);

  await signUp(page, "Eve", `e2e-${Date.now()}@example.com`);

  // Creating an organizer needs only a verified email, but reading its audit
  // log needs a second factor, which this password-only session doesn't have.
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Eve's Events");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Eve's Events")).toBeVisible();
  const before = await readAudit(page);
  expect(before.status).toBe(403);
  expect(before.body).toMatchObject({ error: { code: "mfa_required" } });

  // A password-only session has to confirm the password to add a passkey.
  await addPasskey(page);

  await page.goto("/");
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/signin$/);

  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByText("Eve's Events")).toBeVisible();

  // The passkey sign-in counts as a second factor, so the same request now works.
  const after = await readAudit(page);
  expect(after.status).toBe(200);
  expect(after.body).toContainEqual(expect.objectContaining({ action: "organizer.created" }));
});

function readAudit(page: Page): Promise<{ status: number; body: unknown }> {
  return page.evaluate(async () => {
    const res = await fetch("/api/organizers/eve-s-events/audit");
    return { status: res.status, body: await res.json() };
  });
}
