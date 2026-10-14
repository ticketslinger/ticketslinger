import { expect, test } from "@playwright/test";
import { signUpWithPasskey, virtualAuthenticator } from "./helpers";

test("link an existing Stripe account, disconnect it, and link it again", async ({
  page,
  context,
}) => {
  await virtualAuthenticator(page, context);
  await signUpWithPasskey(page, "Lin", `e2e-link-${Date.now()}@example.com`);

  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Lin Links");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Lin Links")).toBeVisible();

  await page.goto("/o/lin-links/payments");
  await expect(
    page.getByText(
      "Already sell with Stripe? Link that account and the money goes where it always does.",
    ),
  ).toBeVisible();
  // The test provider approves at once and sends us to /stripe/oauth/callback.
  await page.getByRole("button", { name: "Link an existing Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  await expect(page).toHaveURL(/\/o\/lin-links\/payments/);

  await page.getByRole("button", { name: "Disconnect Stripe" }).click();
  await expect(
    page.getByText("Buyers won't be able to pay for tickets until you connect again."),
  ).toBeVisible();
  await page.getByRole("button", { name: "Disconnect Stripe" }).click();
  await expect(page.getByText("Payments are off for now.")).toBeVisible();

  await page.getByRole("button", { name: "Link an existing Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();

  // Opening the callback again with a used state says so, and links nothing new.
  await page.goto("/stripe/oauth/callback?code=fake_code_1&state=used-or-unknown");
  await expect(
    page.getByText("That link has expired. Start again from your organizer's Payments page."),
  ).toBeVisible();
  await page.goto("/stripe/oauth/callback?error=access_denied&state=x");
  await expect(page.getByText("No account was linked, and nothing changed.")).toBeVisible();
});
