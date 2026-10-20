import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import { SuspendedBanner } from "../SuspendedBanner";
import { ApiKeys } from "./ApiKeys";
import { eventTabs } from "./Events";
import { canEdit, navFor, type Org } from "./Organizer";
import { SessionList } from "./Staff";
import {
  ActionDialog,
  ActionLog,
  actionText,
  canConfirm,
  RefundResult,
  waitForRefund,
} from "./StaffActions";

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

test("a suspended owner keeps the read tabs but sees no write controls inside them", () => {
  const o = { role: "owner", slug: "a", name: "A", suspended: true } as Org;
  expect(navFor(o).map((n) => n.label)).toEqual([
    "Events",
    "Payments",
    "Team",
    "API keys",
    "Audit log",
  ]);
  const html = renderToString(createElement(ApiKeys, { org: o }));
  expect(html).not.toContain("Create a key");
  const live = renderToString(createElement(ApiKeys, { org: { ...o, suspended: false } }));
  expect(live).toContain("Create a key");
});

test("the dialog confirm button is on once the reason is good, and the lift copy is plain", () => {
  const dlg = (kind: "lift" | "suspend") =>
    renderToString(
      createElement(ActionDialog, {
        kind,
        organizer: "Acme",
        busy: false,
        error: "Already lifted.",
        onClose: () => {},
        onConfirm: () => {},
      }),
    );
  expect(dlg("lift")).toContain("Their events can sell again and their team can change things.");
  expect(dlg("lift")).toContain("Already lifted.");
  expect(actionText("lift", "Acme")).not.toContain("stay as they were");
  expect(canConfirm("lift", "ten chars!!", "", "")).toBe(true);
});

test("each session shows its actions with reason and refund totals", () => {
  const action = {
    id: "a1",
    kind: "refund_event",
    event: "e",
    reason: "Fraud ring on this event",
    at: "2026-10-20T10:00:00Z",
    finished_at: "2026-10-20T10:01:00Z",
    result: {
      orders: 3,
      amount_cents: 9000,
      fee_cents: 600,
      pending_orders: 0,
      failed_orders: [{ order_id: "ord-9", reason: "payment_used_up" }],
    },
  } as unknown as Parameters<typeof ActionLog>[0]["actions"][number];
  const html = renderToString(createElement(ActionLog, { actions: [action] }));
  expect(html).toContain("Refunded every order of an event");
  expect(html).toContain("Fraud ring on this event");
  expect(html).toContain("Refund these by hand");
  const session = {
    id: "s",
    staff_name: "Sam",
    organizer_name: "Acme",
    reason: "r",
    started_at: "2026-10-20T09:00:00Z",
    ended_how: null,
    end_note: null,
    requests: [],
    actions: [action],
  } as unknown as Parameters<typeof SessionList>[0]["sessions"][number];
  expect(
    renderToString(createElement(SessionList, { sessions: [session], showOrganizer: true })),
  ).toContain("Fraud ring on this event");
});

test("a refund is followed in the staff log until it finishes, and given up on after 11 minutes", async () => {
  const result = {
    orders: 1,
    amount_cents: 5200,
    fee_cents: 200,
    pending_orders: 0,
    failed_orders: [],
  };
  const session = (finished: boolean) =>
    [
      {
        actions: [
          { id: "other", finished_at: "x", result: { ...result, orders: 9 } },
          {
            id: "a1",
            finished_at: finished ? "2026-10-20T10:01:00Z" : null,
            result: finished ? result : null,
          },
        ],
      },
    ] as unknown as Parameters<typeof SessionList>[0]["sessions"];
  let polls = 0;
  let t = 0;
  const sleep = async (ms: number) => {
    t += ms;
  };
  const list = async () => {
    polls++;
    if (polls === 2) throw new Error("network");
    return session(polls >= 4);
  };
  expect(await waitForRefund("a1", list, sleep, () => t)).toEqual(result);
  expect(polls).toBe(4);

  t = 0;
  expect(
    await waitForRefund(
      "a1",
      async () => session(false),
      sleep,
      () => t,
    ),
  ).toBeUndefined();
  expect(t).toBe(11 * 60_000);
});
