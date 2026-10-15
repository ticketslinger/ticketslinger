import { expect, test } from "bun:test";
import { summarize } from "./Orders";

const order = (
  status: "paid" | "cancelled" | "refunded",
  tickets: number,
  total: number,
  fee = 0,
) => ({
  id: crypto.randomUUID(),
  code: "TS-AAAAA",
  email: "a@example.com",
  status,
  ticket_count: tickets,
  total_cents: total,
  fee_cents: fee,
  fee_mode: "pass" as const,
  created_at: "2026-10-01T10:00:00Z",
  paid_at: null,
  refunded_cents: 0,
  fee_refunded_cents: 0,
});

test("summarize counts only paid orders, in integer cents", () => {
  expect(
    summarize([
      order("paid", 2, 5200, 100),
      order("paid", 1, 2599, 50),
      order("cancelled", 4, 9999, 10),
      order("refunded", 3, 7777, 10),
    ]),
  ).toEqual({ orders: 2, tickets: 3, grossCents: 7799, feeCents: 150 });
  expect(summarize([])).toEqual({ orders: 0, tickets: 0, grossCents: 0, feeCents: 0 });
});
