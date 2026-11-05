import { expect, test } from "bun:test";
import type { components } from "@ticketslinger/api-client";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import {
  cancelWarning,
  canRenameTicket,
  canResend,
  emailLine,
  failureText,
  NoteItem,
  RENAME_HINT,
  reasonText,
  refundPreview,
  retryable,
  TicketLine,
} from "./OrderDetail";
import type { Org } from "./Organizer";

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

test("only owners and admins can edit the name of a valid ticket", () => {
  expect(canRenameTicket({ status: "valid" }, { role: "owner" } as Org)).toBe(true);
  expect(canRenameTicket({ status: "valid" }, { role: "admin" } as Org)).toBe(true);
  expect(canRenameTicket({ status: "valid" }, { role: "checkin" } as Org)).toBe(false);
  expect(canRenameTicket({ status: "void" }, { role: "owner" } as Org)).toBe(false);
});

test("rename hint is the spec's", () => {
  expect(RENAME_HINT).toBe(
    "Their QR code doesn't change. The email they already have shows the old name until you resend it.",
  );
});

const line = (over: Partial<Parameters<typeof TicketLine>[0]>) =>
  renderToString(
    createElement(TicketLine, {
      t: ticket("a", 2500, 100, "valid"),
      org: { role: "owner" } as Org,
      path: { organizer: "o", event: "e", order: "x" },
      picked: true,
      busy: false,
      renaming: false,
      refocus: false,
      voidLabel: "",
      refundable: true,
      onPick: () => {},
      onCancel: () => {},
      onEdit: () => {},
      onClose: () => {},
      onSaved: async () => {},
      ...over,
    }),
  );

test("a ticket picked for refund keeps its checked checkbox on screen while its name is edited", () => {
  for (const renaming of [false, true]) {
    const html = line({ renaming });
    expect(html).toMatch(/<input type="checkbox"[^>]*checked/);
  }
  expect(line({ renaming: true })).toContain('maxLength="120"');
});

test("Edit name is off while a refund is going through, and absent while editing", () => {
  expect(line({ busy: true })).toMatch(/<button[^>]*disabled[^>]*>Edit name/);
  expect(line({ busy: false })).not.toMatch(/<button[^>]*disabled[^>]*>Edit name/);
  expect(line({ renaming: true })).not.toContain("Edit name");
});

test("a comp or manual order's tickets have no refund checkbox, and still a More menu", () => {
  const html = line({ refundable: false, renaming: false });
  expect(html).not.toContain('type="checkbox"');
  expect(html).toContain("Edit name");
  expect(html).toMatch(/<summary[^>]*>More/);
  expect(html).toContain("Cancel ticket");
  expect(line({ refundable: false, renaming: true })).not.toContain('type="checkbox"');
});

test("the cancel warning says what happens to the money, and whether the buyer hears", () => {
  expect(cancelWarning("online", 5000, true)).toBe(
    "Nothing is refunded: the buyer's $50.00 stays paid. Their tickets stop working straight away. We'll email the buyer to say so.",
  );
  expect(cancelWarning("manual", 4000, false)).toBe(
    "Nothing is refunded here. If they paid you $40.00, give it back yourself. Their tickets stop working straight away. The buyer won't be told.",
  );
  expect(cancelWarning("comp", 0, true)).toBe(
    "They paid nothing, so there's nothing to refund. Their tickets stop working straight away. We'll email the buyer to say so.",
  );
});

test("NoteItem renders text", () => {
  const html = renderToString(
    createElement(NoteItem, {
      note: {
        id: "n",
        author_name: "Sam",
        body: "<img src=x onerror=alert(1)>",
        created_at: "2026-11-05T12:00:00Z",
        can_delete: true,
      },
      timezone: "UTC",
      canDelete: true,
      onDeleted: () => {},
      path: { organizer: "o", event: "e", order: "x" },
    }),
  );
  expect(html).toContain("&lt;img");
  expect(html).not.toContain("<img src=x");
  expect(html).toContain("Sam");
  expect(html).toContain("Delete");
});
