import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { EMPTY_REPORT, exportHref, figures } from "./Report";

type EventReport = components["schemas"]["EventReport"];

const fixture = {
  currency: "USD",
  gross_cents: 125000,
  refunded_cents: 2500,
  platform_fee_cents: 4850,
  tax_cents: 0,
  net_cents: 117650,
  orders: 10,
  sold: 20,
  checked_in: 3,
  ticket_types: [],
  quotas: [],
  sales_by_day: [],
  promo_codes: [],
} as EventReport;

test("the four figures, in order, as dollars", () => {
  expect(figures(fixture)).toEqual([
    ["Ticket sales", "$1,250.00"],
    ["Refunded", "$25.00"],
    ["Our fee", "$48.50"],
    ["Yours before Stripe's fees", "$1,176.50"],
  ]);
});

test("empty state copy", () => {
  expect(EMPTY_REPORT).toBe(
    "No sales yet. The figures fill in as soon as the first order comes in.",
  );
});

test("download links go through the dashboard proxy", () => {
  expect(
    (["orders.csv", "attendees.csv", "checkins.csv"] as const).map((k) =>
      exportHref("acme", "e1", k),
    ),
  ).toEqual([
    "/api/organizers/acme/events/e1/exports/orders.csv",
    "/api/organizers/acme/events/e1/exports/attendees.csv",
    "/api/organizers/acme/events/e1/exports/checkins.csv",
  ]);
});
