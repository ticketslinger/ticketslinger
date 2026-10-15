import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { refundPreview } from "./OrderDetail";

type Detail = components["schemas"]["OrderDetail"];

const ticket = (id: string, charge: number, fee: number, status: "valid" | "void") => ({
  id,
  ticket_type_name: "General",
  attendee_name: id,
  status,
  charge_cents: charge,
  fee_cents: fee,
  refund_id: null,
});
const order = (mode: "pass" | "absorb") =>
  ({
    fee_mode: mode,
    tickets: [
      ticket("a", 2500, 100, "valid"),
      ticket("b", 1000, 70, "valid"),
      ticket("c", 2500, 100, "void"),
    ],
  }) as Detail;

test("refund preview follows the API's money rule", () => {
  expect(refundPreview(order("pass"), [], true)).toEqual({
    tickets: 2,
    amountCents: 3670,
    feeCents: 170,
  });
  expect(refundPreview(order("pass"), ["b"], false)).toEqual({
    tickets: 1,
    amountCents: 1000,
    feeCents: 0,
  });
  expect(refundPreview(order("absorb"), ["a"], true)).toEqual({
    tickets: 1,
    amountCents: 2500,
    feeCents: 100,
  });
});
