import { devices, expect, test } from "@playwright/test";
import { createSellableEvent, signUp, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const SHOP = "http://localhost:3003";

test("door staff find a guest by name and check them in", async ({ page, context, browser }) => {
  await virtualAuthenticator(page, context);
  const stamp = Date.now();
  await signUpWithPasskey(page, "Oscar", `e2e-door-owner-${stamp}@example.com`);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill(`Oscar Door ${stamp}`);
  await page.getByRole("button", { name: "Create organizer" }).click();
  const slug = `oscar-door-${stamp}`;
  await expect(page.getByText(`Oscar Door ${stamp}`)).toBeVisible();
  await page.goto(`/o/${slug}/payments`);
  await page.getByRole("button", { name: "Create a Stripe account" }).click();
  await expect(page.getByText("You're ready to take payments.")).toBeVisible();
  await createSellableEvent(page, slug);

  // Lin takes a free Volunteer ticket.
  const buyer = await browser.newContext({ ...devices["Pixel 7"] });
  const shop = await buyer.newPage();
  await shop.goto(`${SHOP}/${slug}/spring-social`);
  await shop.getByLabel("Volunteer").selectOption("1");
  await expect(shop.locator('input[name="cf-turnstile-response"]')).not.toHaveValue("", {
    timeout: 15_000,
  });
  await shop.getByRole("button", { name: "Get tickets" }).click();
  await shop.getByLabel("Email").fill(`lin-${stamp}@example.com`);
  await shop.getByLabel("Name on ticket 1").fill("Lin");
  await shop.getByLabel("Dietary needs").fill("Vegan");
  await shop.getByRole("button", { name: "Get my free ticket" }).click();
  await expect(shop).toHaveURL(/\/orders\/TS-/);
  await buyer.close();

  // Someone signs up for the door shift, with a password and nothing more.
  const staffContext = await browser.newContext({
    ...devices["Pixel 7"],
    baseURL: test.info().project.use.baseURL,
  });
  const staff = await staffContext.newPage();
  const doorEmail = `door-${stamp}@example.com`;
  await signUp(staff, "Dot", doorEmail);

  // The owner adds them to the team as check-in staff.
  await page.goto(`/o/${slug}/team`);
  await page.getByLabel("Email").fill(doorEmail);
  await page.getByLabel("Role").selectOption("checkin");
  await page.getByRole("button", { name: "Add to team" }).click();
  await expect(page.getByRole("cell", { name: doorEmail })).toBeVisible();

  await staff.goto(`/o/${slug}`);
  await staff.getByRole("link", { name: "Spring Social" }).click();
  await staff.getByRole("link", { name: "Check-in" }).click();
  await staff.getByLabel("Find a guest").fill("lin");
  await staff.getByRole("button", { name: "Check in Lin" }).click();
  await expect(staff.getByRole("heading", { name: "Welcome in" })).toBeVisible();

  // Money and the team stay with the owner.
  await expect(staff.getByRole("link", { name: "Report" })).toHaveCount(0);
  await expect(staff.getByRole("link", { name: "Orders" })).toHaveCount(0);
  await expect(staff.getByRole("link", { name: "Team" })).toHaveCount(0);
  await staffContext.close();
});
