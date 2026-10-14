import { expect, test } from "@playwright/test";
import { API, PASSWORD, signUpWithPasskey, virtualAuthenticator } from "./helpers";

const PHONE = { width: 390, height: 844 };
const DESKTOP = { width: 1280, height: 900 };

test("build a sellable event, reach it with an API key, then get back in with a recovery code", async ({
  page,
  context,
  request,
}) => {
  await virtualAuthenticator(page, context);
  const email = `e2e-catalogue-${Date.now()}@example.com`;
  await signUpWithPasskey(page, "Cass", email);

  // Editing happens on a desktop screen.
  await page.setViewportSize(DESKTOP);
  await page.goto("/organizers/new");
  await page.getByLabel("Organizer name").fill("Social Club");
  await page.getByRole("button", { name: "Create organizer" }).click();
  await expect(page.getByText("Social Club")).toBeVisible();

  await page.goto("/o/social-club/events/new");
  await page.getByLabel("Event name").fill("Spring Social");
  await page.getByLabel("Time zone").selectOption("America/Denver");
  await page.getByLabel("Starts").fill("2026-05-02T19:00");
  await page.getByLabel("Ends").fill("2026-05-02T23:00");
  await page.getByRole("button", { name: "Create event" }).click();
  await expect(page.getByRole("heading", { name: "Spring Social" })).toBeVisible();

  // Nothing to sell yet, so going live is refused with a reason.
  await page.getByRole("button", { name: "Go live" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Add at least one ticket type" }),
  ).toBeVisible();

  await page.getByRole("link", { name: "Tickets" }).click();
  await page.getByRole("button", { name: "Add a ticket" }).click();
  await page.getByLabel("Name").fill("General");
  await page.getByLabel("Price (USD)").fill("25.00");
  await page.getByRole("button", { name: "Add ticket" }).click();
  await expect(page.getByRole("cell", { name: "$25.00" })).toBeVisible();

  await page.getByRole("button", { name: "Add a ticket" }).click();
  await page.getByLabel("Name").fill("VIP");
  await page.getByLabel("Price (USD)").fill("60.00");
  await page.getByLabel("Hidden").check();
  await page.getByRole("button", { name: "Add ticket" }).click();
  await expect(page.getByRole("cell", { name: "$60.00" })).toBeVisible();

  await page.getByRole("button", { name: "Add a capacity" }).click();
  await page.getByLabel("Name").fill("Venue");
  await page.getByLabel("How many tickets").fill("120");
  await page.getByRole("checkbox", { name: "General" }).check();
  await page.getByRole("checkbox", { name: "VIP" }).check();
  await page.getByRole("button", { name: "Add capacity" }).click();
  await expect(page.getByRole("cell", { name: "120", exact: true })).toBeVisible();

  await page.getByRole("button", { name: "Add a capacity" }).click();
  await page.getByLabel("Name").fill("VIP");
  await page.getByLabel("How many tickets").fill("20");
  await page.getByRole("checkbox", { name: "VIP" }).check();
  await page.getByRole("button", { name: "Add capacity" }).click();
  await expect(page.getByRole("cell", { name: "20", exact: true })).toBeVisible();

  await page.getByRole("link", { name: "Promo codes" }).click();
  await page.getByRole("button", { name: "Add a promo code" }).click();
  await page.getByLabel("Code", { exact: true }).fill("friends");
  await page.getByLabel("Percent off").fill("10");
  await page.getByLabel("Only the tickets I pick").check();
  await page
    .getByRole("group", { name: "Applies to" })
    .getByRole("checkbox", { name: "General" })
    .check();
  await page
    .getByRole("group", { name: "Shows hidden tickets" })
    .getByRole("checkbox", { name: "VIP" })
    .check();
  await page.getByRole("button", { name: "Add promo code" }).click();
  await expect(page.getByRole("row", { name: /FRIENDS 10% off/ })).toBeVisible();

  await page.getByRole("link", { name: "Questions" }).click();
  await page.getByRole("button", { name: "Add a question" }).click();
  await page.getByLabel("Question", { exact: true }).fill("Name on ticket");
  await page.getByLabel("Answer with").selectOption("text");
  await page.getByLabel("Buyers have to answer").check();
  await page.getByLabel("For every ticket").check();
  await page.getByRole("button", { name: "Add question" }).click();
  await expect(
    page.getByRole("row", { name: /Name on ticket Required Text box For every ticket/ }),
  ).toBeVisible();

  await page.getByRole("button", { name: "Go live" }).click();
  await expect(page.getByRole("button", { name: "Back to draft" })).toBeVisible();
  // The pill reads "On sale" in the dashboard; the API calls it live.
  await expect(page.locator(".pill.live")).toHaveText("On sale");

  // On a phone, the read views still show what was built.
  await page.setViewportSize(PHONE);
  await page.goto("/o/social-club");
  await expect(page.getByRole("row", { name: /Spring Social.*On sale/ })).toBeVisible();
  await page.getByRole("link", { name: "Spring Social" }).click();
  await expect(page.getByText("This event is on sale.")).toBeVisible();
  await page.getByRole("link", { name: "Tickets" }).click();
  await expect(page.getByRole("row", { name: /VIP.*\$60\.00/ })).toBeVisible();
  await page.setViewportSize(DESKTOP);

  // A read-only key: it lists the event, can't change anything, and dies when revoked.
  await page.goto("/o/social-club/api-keys");
  await page.getByLabel("Key name").fill("Box office laptop");
  await page.getByRole("button", { name: "Create key" }).click();
  const key = await page.getByLabel("Your new key").inputValue();
  expect(key).toMatch(/^tsk_/);
  const auth = { Authorization: `Bearer ${key}` };
  const events = `${API}/v1/organizers/social-club/events`;

  const get = async (path: string) => {
    const res = await request.get(`${events}${path}`, { headers: auth });
    expect(res.status()).toBe(200);
    return res.json();
  };
  const list = await get("");
  expect(list).toContainEqual(expect.objectContaining({ name: "Spring Social" }));
  const ev = list.find((e: { name: string }) => e.name === "Spring Social");
  expect(ev).toMatchObject({
    status: "live",
    timezone: "America/Denver",
  });
  // The API writes the instants in the event's own offset (-06:00 in May); compare the moments.
  expect(Date.parse(ev.starts_at)).toBe(Date.parse("2026-05-03T01:00:00Z"));
  expect(Date.parse(ev.ends_at)).toBe(Date.parse("2026-05-03T05:00:00Z"));
  const base = `/${ev.id}`;
  const types: { id: string; name: string; hidden: boolean }[] = await get(`${base}/ticket-types`);
  const id = (name: string) => types.find((t) => t.name === name)?.id;
  expect(types.map((t) => [t.name, t.hidden])).toEqual([
    ["General", false],
    ["VIP", true],
  ]);
  const quotas: { name: string; size: number; ticket_type_ids: string[] }[] = await get(
    `${base}/quotas`,
  );
  const quota = (name: string) => quotas.find((q) => q.name === name);
  expect(quota("Venue")?.size).toBe(120);
  expect([...(quota("Venue")?.ticket_type_ids ?? [])].sort()).toEqual(
    [id("General"), id("VIP")].sort(),
  );
  expect(quota("VIP")?.size).toBe(20);
  expect(quota("VIP")?.ticket_type_ids).toEqual([id("VIP")]);
  expect(await get(`${base}/promo-codes`)).toEqual([
    expect.objectContaining({
      code: "FRIENDS",
      kind: "percent",
      value: 1000,
      applies_to_all: false,
      applies_to: [id("General")],
      unlocks: [id("VIP")],
    }),
  ]);
  const posted = await request.post(events, { headers: auth, data: {} });
  expect(posted.status()).toBe(403);
  expect(await posted.json()).toMatchObject({ error: { code: "scope_read_only" } });

  await page.getByRole("button", { name: "Revoke Box office laptop" }).click();
  await page.getByRole("button", { name: "Revoke Box office laptop" }).click();
  await expect(page.getByText("No keys yet")).toBeVisible();
  expect((await request.get(events, { headers: auth })).status()).toBe(401);

  // Recovery codes: make them, sign out, and come back through a deep link.
  await page.goto("/security");
  await page.getByRole("button", { name: "Create recovery codes" }).click();
  const code =
    (await page
      .getByRole("list", { name: "Your recovery codes" })
      .getByRole("listitem")
      .first()
      .textContent()) ?? "";
  expect(code).toMatch(/^[A-Z0-9]{4}(-[A-Z0-9]{4}){2}$/);
  await page.goto("/");
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/signin$/);

  await page.goto("/o/social-club");
  await expect(page).toHaveURL(/\/signin\?next=%2Fo%2Fsocial-club$/);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Use a recovery code instead" }).click();
  await page.getByLabel("Recovery code").fill(code);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(/\/o\/social-club$/);
  await expect(page.getByRole("heading", { name: "Events" })).toBeVisible();
});
