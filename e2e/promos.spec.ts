import { expect, test } from "@playwright/test";

const API = process.env.E2E_API ?? "http://127.0.0.1:3001";

test("promo codes and questions: normalised codes, inline errors, reordering", async ({
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

  const email = `e2e-promos-${Date.now()}@example.com`;
  await page.goto("/signup");
  await page.getByLabel("Name").fill("Pat");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("a long test passphrase");
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByText("Check your inbox")).toBeVisible();
  const mail = await fetch(`${API}/__test/last-email?to=${encodeURIComponent(email)}`);
  await page.goto(await mail.text());
  await expect(page.getByText("Your email is verified")).toBeVisible();

  // Writing needs a second factor: add a passkey, then sign in with it.
  await page.goto("/security");
  await page.getByLabel("Password").fill("a long test passphrase");
  await page.getByRole("button", { name: "Add a passkey" }).click();
  await expect(page.getByText("Passkey added")).toBeVisible();
  await page.goto("/");
  await page.getByRole("button", { name: "Sign out" }).click();
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(/\/$/);

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
