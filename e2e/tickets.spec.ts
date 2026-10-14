import { expect, test } from "@playwright/test";
import { signUpWithPasskey, virtualAuthenticator } from "./helpers";

test("add capacity and a ticket to a draft event, with a bad price caught inline", async ({
  page,
  context,
}) => {
  await virtualAuthenticator(page, context);
  await signUpWithPasskey(page, "Tess", `e2e-tickets-${Date.now()}@example.com`);

  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Tess Tickets");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Tess Tickets")).toBeVisible();

  await page.goto("/o/tess-tickets/events/new");
  await page.getByLabel("Event name").fill("Spring Fair");
  await page.getByLabel("Starts").fill("2027-05-01T10:00");
  await page.getByLabel("Ends").fill("2027-05-01T18:00");
  await page.getByRole("button", { name: "Create event" }).click();
  await page.getByRole("link", { name: "Tickets" }).click();

  await page.getByRole("button", { name: "Add a capacity" }).click();
  await page.getByLabel("Name").fill("Whole fair");
  await page.getByLabel("How many tickets").fill("1,200");
  await page.getByRole("button", { name: "Add capacity" }).click();
  await expect(page.getByRole("cell", { name: "1,200" })).toBeVisible();

  await page.getByRole("button", { name: "Add a ticket" }).click();
  await page.getByLabel("Name").fill("General");
  await page.getByLabel("Price (USD)").fill("12.345");
  await page.getByRole("button", { name: "Add ticket" }).click();
  await expect(page.getByLabel("Price (USD)")).toHaveAttribute("aria-invalid", "true");
  await page.getByLabel("Price (USD)").fill("$12.50");
  await page.getByRole("checkbox", { name: /Whole fair/ }).check();
  await page.getByRole("button", { name: "Add ticket" }).click();
  await expect(page.getByRole("cell", { name: "$12.50" })).toBeVisible();
  await expect(page.getByRole("row", { name: /Whole fair 1,200 General/ })).toBeVisible();

  // A second ticket goes to the end; one click puts it first and focus goes with it.
  await page.getByRole("button", { name: "Add a ticket" }).click();
  await page.getByLabel("Name").fill("VIP");
  await page.getByLabel("Price (USD)").fill("40");
  await page.getByRole("checkbox", { name: /Whole fair/ }).check();
  await page.getByRole("button", { name: "Add ticket" }).click();
  const tickets = page
    .getByRole("row")
    .filter({ hasText: /General|VIP/ })
    .filter({ hasText: "$" });
  await expect(tickets.nth(0)).toContainText("General");
  await page.getByRole("button", { name: "Move VIP up" }).click();
  await expect(tickets.nth(0)).toContainText("VIP");
  await expect(page.getByRole("button", { name: "Move VIP up" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Move VIP down" })).toBeFocused();
});
