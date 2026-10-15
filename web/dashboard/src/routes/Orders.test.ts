import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { summarize } from "./Orders";

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
  });
  expect(summarize([]).netCents).toBe(0);
});
