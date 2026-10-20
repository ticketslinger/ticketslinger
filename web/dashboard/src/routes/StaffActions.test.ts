import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import { SuspendedBanner } from "../SuspendedBanner";
import { eventTabs } from "./Events";
import { canEdit, type Org } from "./Organizer";
import { ActionDialog, actionText, canConfirm, RefundResult } from "./StaffActions";

const reason = "Chargebacks all week";

test("confirm needs a 10 character reason; a refund also needs the event's name", () => {
  expect(canConfirm("suspend", "too short", "", "")).toBe(false);
  expect(canConfirm("suspend", reason, "", "")).toBe(true);
  expect(canConfirm("lift", reason, "", "")).toBe(true);
  expect(canConfirm("refund_event", reason, "", "Gala")).toBe(false);
  expect(canConfirm("refund_event", reason, "gala", "Gala")).toBe(false);
  expect(canConfirm("refund_event", reason, " Gala ", "Gala")).toBe(true);
  expect(canConfirm("refund_event", "short", "Gala", "Gala")).toBe(false);
});

test("the dialog starts with its confirm button off and shows the spec copy", () => {
  const html = renderToString(
    createElement(ActionDialog, {
      kind: "refund_event",
      organizer: "Acme",
      event: { id: "e", name: "Gala" },
      busy: false,
      error: "",
      onClose: () => {},
      onConfirm: () => {},
    }),
  );
  expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Touch your passkey to confirm/);
  expect(html).toContain("Type the event&#x27;s name to confirm");
  expect(html).toContain("You can&#x27;t undo this.");
});

test("dialog text is the spec's", () => {
  expect(actionText("suspend", "Acme")).toBe(
    "Suspend Acme? Every event goes off sale now, and their team can't change anything until you lift it. Paid orders stay as they are.",
  );
  expect(actionText("refund_event", "Acme", "Gala")).toBe(
    "Refund every order for Gala? Each buyer gets all their money back, our fee included. You can't undo this.",
  );
});

test("the suspended banner is a persistent alert with the spec copy and a mailto link", () => {
  const html = renderToString(createElement(SuspendedBanner, { organizer: "Acme" }));
  expect(html).toContain('role="alert"');
  expect(html).toContain(
    "We&#x27;ve suspended Acme. Your events are off sale and nothing here can be changed, but all your data is still here. Want to talk about it? Email ",
  );
  expect(html).toContain('href="mailto:support@ticketslinger.com"');
  expect(html).not.toContain("<button");
});

test("a suspended organizer hides every write control, even from owners", () => {
  const o = (suspended: boolean) => ({ role: "owner", slug: "a", name: "A", suspended }) as Org;
  expect(canEdit(o(false))).toBe(true);
  expect(canEdit(o(true))).toBe(false);
  expect(eventTabs(o(false)).map((t) => t.label)).toContain("Check-in");
  expect(eventTabs(o(true)).map((t) => t.label)).not.toContain("Check-in");
});

test("a refund result lists failed orders to refund by hand", () => {
  const base = { orders: 2, amount_cents: 5000, fee_cents: 300, pending_orders: 1 };
  const bad = renderToString(
    createElement(RefundResult, {
      result: { ...base, failed_orders: [{ order_id: "ord-1", reason: "payment_used_up" }] },
    }),
  );
  expect(bad).toContain("Refund these by hand");
  expect(bad).toContain("ord-1");
  expect(bad).toContain("payment_used_up");
  const ok = renderToString(
    createElement(RefundResult, { result: { ...base, failed_orders: [] } }),
  );
  expect(ok).not.toContain("by hand");
  expect(ok).toContain("Refunded 2 orders");
});
