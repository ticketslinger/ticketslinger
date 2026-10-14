import { expect, test } from "@playwright/test";
import { signUpWithPasskey, virtualAuthenticator } from "./helpers";

test("connect Stripe, come back ready, and see the empty Orders tab", async ({ page, context }) => {
  await virtualAuthenticator(page, context);
  await signUpWithPasskey(page, "Pia", `e2e-pay-${Date.now()}@example.com`);

  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Pia Pays");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Pia Pays")).toBeVisible();

  await page.goto("/o/pia-pays");
  await page.getByRole("link", { name: "Payments" }).click();
  await expect(page.getByText("Connect Stripe to sell paid tickets.")).toBeVisible();
  // The test provider sends us straight back with ?return=1.
  await page.getByRole("button", { name: "Connect Stripe" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();

  await page.goto("/o/pia-pays/events/new");
  await page.getByLabel("Event name").fill("Opening Night");
  await page.getByLabel("Starts").fill("2027-05-01T10:00");
  await page.getByLabel("Ends").fill("2027-05-01T18:00");
  await page.getByRole("button", { name: "Create event" }).click();
  await page.getByRole("link", { name: "Orders" }).click();
  await expect(
    page.getByText("No orders yet. Once someone buys, they'll show up here."),
  ).toBeVisible();
});
