import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import {
  canResend,
  emailLine,
  failureText,
  reasonText,
  refundPreview,
  retryable,
} from "./OrderDetail";

type Detail = components["schemas"]["OrderDetail"];
type Refund = components["schemas"]["Refund"];
type OrderEmail = components["schemas"]["OrderEmail"];

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

test("a refund we gave up on says we couldn't reach Stripe, not that Stripe said no", () => {
  const r = { status: "failed", failure: "gave_up", returned_by_bank: false } as Refund;
  expect(failureText(r)).toBe(
    "We couldn't get this through to Stripe, so we stopped trying. Try again, or refund it from your Stripe dashboard.",
  );
});

test("a refund the bank sent back can't be retried from here", () => {
  const r = { status: "failed", failure: "stripe_failed", returned_by_bank: true } as Refund;
  expect(retryable(r)).toBe(false);
  expect(retryable({ ...r, returned_by_bank: false })).toBe(true);
  expect(retryable({ ...r, status: "succeeded", failure: null, returned_by_bank: false })).toBe(
    false,
  );
});

test("a late payment on a cancelled event reads as the cancellation", () => {
  const r = { reason: "late_payment" } as Refund;
  expect(reasonText(r, true)).toBe("Event cancelled");
  expect(reasonText(r, false)).toBe("Paid after the tickets had gone");
  expect(reasonText({ ...r, reason: "organizer" }, true)).toBe("You refunded it");
});

test("the ticket email line says how the email is getting on, in the spec's words", () => {
  const e = (
    status: OrderEmail["status"],
    attempts = 0,
    problem: OrderEmail["problem"] = null,
  ): OrderEmail => ({ status, problem, attempts, updated_at: "2026-10-19T18:00:00Z" });
  const when = (iso: string) => `at ${iso.slice(11, 16)}`;
  expect(emailLine(e(null), when)).toBeNull();
  expect(emailLine(e("queued"), when)).toBe("On its way.");
  expect(emailLine(e("sent", 1), when)).toBe("Sent at 18:00.");
  expect(emailLine(e("delivered", 1), when)).toBe("Delivered at 18:00.");
  expect(emailLine(e("failing", 3, "quota"), when)).toBe(
    "Not sent yet. We've tried 3 times and we'll keep going for a while. Last problem: we've hit our sending limit for now.",
  );
  expect(emailLine(e("failing", 1, "unreachable"), when)).toBe(
    "Not sent yet. We've tried once and we'll keep going for a while. Last problem: we couldn't reach the mail service.",
  );
  expect(emailLine(e("gave_up", 1, "bad_address"), when)).toBe(
    "We gave up after one try. Last problem: that address can't take mail.",
  );
  expect(emailLine(e("gave_up", 10, "rate_limited"), when)).toBe(
    "We gave up after 10 tries. Last problem: the mail service asked us to slow down.",
  );
  expect(emailLine(e("gave_up", 2, "refused"), when)).toBe(
    "We gave up after 2 tries. Last problem: the mail service turned the message down.",
  );
  expect(emailLine(e("failing", 2, "config"), when)).toBe(
    "Not sent yet. We've tried 2 times and we'll keep going for a while. Last problem: our mail settings need fixing (that one's on us).",
  );
  expect(emailLine(e("gave_up", 10), when)).toBe("We gave up after 10 tries.");
  expect(emailLine(e("bounced", 1), when)).toBe(
    "Bounced. The buyer's mail server turned it away, so the address may have a typo. Check it with them.",
  );
  expect(emailLine(e("complained", 1), when)).toBe("Delivered, then the buyer marked it as spam.");
});

test("the tickets can be sent again only once the last email has settled", () => {
  const e = (status: OrderEmail["status"]): OrderEmail => ({
    status,
    problem: null,
    attempts: 1,
    updated_at: null,
  });
  for (const s of ["sent", "delivered", "gave_up", "bounced", "complained"] as const) {
    expect(canResend(e(s))).toBe(true);
  }
  for (const s of ["queued", "failing", null] as const) {
    expect(canResend(e(s))).toBe(false);
  }
});
