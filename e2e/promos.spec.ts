import { expect, test } from "@playwright/test";
import { signUpWithPasskey, virtualAuthenticator } from "./helpers";

test("promo codes and questions: normalised codes, inline errors, reordering", async ({
  page,
  context,
}) => {
  await virtualAuthenticator(page, context);
  await signUpWithPasskey(page, "Pat", `e2e-promos-${Date.now()}@example.com`);

  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Pat Promos");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Pat Promos")).toBeVisible();

  await page.goto("/o/pat-promos/events/new");
  await page.getByLabel("Event name").fill("Autumn Fair");
  await page.getByLabel("Starts").fill("2027-10-01T10:00");
  await page.getByLabel("Ends").fill("2027-10-01T18:00");
  await page.getByRole("button", { name: "Create event" }).click();

  await page.getByRole("link", { name: "Promo codes" }).click();
  await page.getByRole("button", { name: "Add a promo code" }).click();
  await page.getByLabel("Code", { exact: true }).fill("spring25");
  await expect(page.getByLabel("Code", { exact: true })).toHaveValue("SPRING25");
  await page.getByLabel("Percent off").fill("100.5");
  await page.getByRole("button", { name: "Add promo code" }).click();
  await expect(page.getByLabel("Percent off")).toHaveAttribute("aria-invalid", "true");
  await page.getByLabel("Percent off").fill("12.5");
  await page.getByRole("button", { name: "Add promo code" }).click();
  await expect(page.getByRole("row", { name: /SPRING25 12.5% off 0 \/ no limit/ })).toBeVisible();
  await page.getByRole("button", { name: "Add a promo code" }).click();
  await page.getByLabel("Code", { exact: true }).fill(" Spring25 ");
  await page.getByLabel("Percent off").fill("5");
  await page.getByRole("button", { name: "Add promo code" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "already has that code" })).toBeVisible();
  await expect(page.getByLabel("Code", { exact: true })).toBeFocused();
  await page.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("button", { name: "Leave without saving" }).click();

  await page.getByRole("link", { name: "Questions" }).click();
  for (const label of ["Company name", "Meal"]) {
    await page.getByRole("button", { name: "Add a question" }).click();
    await page.getByLabel("Question", { exact: true }).fill(label);
    if (label === "Meal") {
      await page.getByLabel("Answer with").selectOption("select");
      await page.getByRole("textbox", { name: "Option 1", exact: true }).fill("Veg");
      await page.getByRole("textbox", { name: "Option 2", exact: true }).fill("veg");
      await page.getByRole("button", { name: "Add question" }).click();
      await expect(page.getByRole("textbox", { name: "Option 2", exact: true })).toBeFocused();
      await expect(page.getByRole("textbox", { name: "Option 2", exact: true })).toHaveAttribute(
        "aria-invalid",
        "true",
      );
      await expect(
        page.getByRole("textbox", { name: "Option 2", exact: true }),
      ).toHaveAccessibleDescription(/listed twice/);
      await page.getByRole("textbox", { name: "Option 2", exact: true }).fill("Vegan");
    }
    await page.getByRole("button", { name: "Add question" }).click();
    await expect(page.getByRole("cell", { name: label, exact: true })).toBeVisible();
  }
  const rows = page.getByRole("row");
  await expect(rows.nth(1)).toContainText("Company name");
  await page.getByRole("button", { name: "Move Meal up" }).click();
  await expect(rows.nth(1)).toContainText("Meal");
  await expect(page.getByRole("button", { name: "Move Meal up" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Move Meal down" })).toBeFocused();
});
