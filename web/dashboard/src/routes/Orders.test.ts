import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { emailTag, sourceBadge, summarize } from "./Orders";

type OrderSummary = components["schemas"]["OrderSummary"];

test("summary: gross, refunds, our fee and net, in cents", () => {
  const o = (
    status: OrderSummary["status"],
    total: number,
    fee: number,
    refunded = 0,
    feeBack = 0,
    tickets = 2,
  ) =>
    ({
      status,
      total_cents: total,
      fee_cents: fee,
      refunded_cents: refunded,
      fee_refunded_cents: feeBack,
      ticket_count: tickets,
      source: "online",
      manual_cents: 0,
    }) as OrderSummary;
  const sum = summarize([
    o("paid", 5200, 200),
    o("partially_refunded", 5200, 200, 2600, 100, 1),
    o("refunded", 5200, 200, 5200, 200, 0),
    o("cancelled", 2600, 100, 2600, 100, 0),
  ]);
  expect(sum).toEqual({
    orders: 2,
    tickets: 3,
    grossCents: 18200,
    refundedCents: 10400,
    feeCents: 300,
    netCents: 7500,
    manualCents: 0,
    compTickets: 0,
  });
  expect(summarize([]).netCents).toBe(0);
});

test("the list tags orders whose ticket email didn't get through", () => {
  const o = (status: OrderSummary["ticket_email"]["status"]) =>
    ({ ticket_email: { status, problem: null, attempts: 1, updated_at: null } }) as OrderSummary;
  expect(emailTag(o("failing"))).toBe("Email failing");
  expect(emailTag(o("gave_up"))).toBe("Email not sent");
  expect(emailTag(o("bounced"))).toBe("Email bounced");
  for (const s of ["queued", "sent", "delivered", "complained", null] as const) {
    expect(emailTag(o(s))).toBeNull();
  }
});

test("the source badge names comp and manual orders, never online ones", () => {
  expect(sourceBadge({ source: "online", manual_method: null })).toBeNull();
  expect(sourceBadge({ source: "comp", manual_method: null })).toBe("Comp");
  expect(sourceBadge({ source: "manual", manual_method: "cash" })).toBe("Manual · cash");
});

test("summary keeps money taken by hand and comps out of sales", () => {
  const o = (source: OrderSummary["source"], total: number, tickets: number, manual = 0) =>
    ({
      source,
      status: "paid",
      total_cents: total,
      fee_cents: 0,
      refunded_cents: 0,
      fee_refunded_cents: 0,
      ticket_count: tickets,
      manual_cents: manual,
    }) as OrderSummary;
  // The manual order had a $20 ticket cancelled: only the one left counts, as on the Report.
  const sum = summarize([o("online", 2600, 1), o("manual", 4000, 1, 2000), o("comp", 0, 2)]);
  expect(sum.grossCents).toBe(2600);
  expect(sum.manualCents).toBe(2000);
  expect(sum.compTickets).toBe(2);
  expect(sum.netCents).toBe(2600);
});
