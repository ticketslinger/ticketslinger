import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { failureText, refundPreview } from "./OrderDetail";

type Detail = components["schemas"]["OrderDetail"];
type Refund = components["schemas"]["Refund"];

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

test("a refund the buyer's bank sent back tells the organizer to contact the buyer", () => {
  const r = { status: "failed", failure: "stripe_failed", returned_by_bank: true } as Refund;
  expect(failureText(r)).toContain("the buyer's bank sent it back");
  expect(failureText(r)).toContain("Contact the buyer");
  expect(failureText({ ...r, returned_by_bank: false })).toBe("Stripe couldn't send it.");
  expect(failureText({ ...r, failure: null, returned_by_bank: false })).toBe(
    "Stripe couldn't send it.",
  );
});
