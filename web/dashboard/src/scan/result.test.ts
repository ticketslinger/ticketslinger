import { expect, test } from "bun:test";
import type { CheckinResult } from "./result";
import { shouldSend, viewFor } from "./result";

const ticket = {
  id: "0192f0a0-0000-7000-8000-000000000001",
  ticket_type_name: "General admission",
  attendee_name: "Ana Ruiz",
  email: "ana@example.com",
  order_code: "TS-7KQ2MX",
  status: "valid" as const,
  checked_in_at: "2027-03-02T02:42:00Z",
};
const r = (
  result: CheckinResult["result"],
  t: CheckinResult["ticket"] = ticket,
): CheckinResult => ({
  result,
  ticket: t,
  checked_in: 1,
  sold: 2,
});
const ZONE = "America/Denver";

test("accepted is green, names the guest and dismisses itself", () => {
  expect(viewFor(r("accepted"), ZONE)).toEqual({
    tone: "green",
    heading: "Welcome in",
    body: "Ana Ruiz · General admission",
    action: null,
    autoDismissMs: 2500,
  });
  expect(viewFor(r("accepted", { ...ticket, attendee_name: "" }), ZONE).body).toBe(
    "Guest · General admission",
  );
});

test("duplicate is amber and says when, in the event's zone", () => {
  expect(viewFor(r("duplicate"), ZONE)).toEqual({
    tone: "amber",
    heading: "Already checked in",
    body: "This ticket came in at 7:42 PM. Make sure it's the same person before you let them through.",
    action: "Scan the next ticket",
    autoDismissMs: null,
  });
});

test("invalid and void are red and wait for a tap", () => {
  expect(viewFor(r("invalid", null), ZONE)).toEqual({
    tone: "red",
    heading: "Not a ticket for this event",
    body: "It may be for another event, or not one of ours. Ask to see the ticket email or PDF.",
    action: "Scan the next ticket",
    autoDismissMs: null,
  });
  expect(viewFor(r("void", { ...ticket, status: "void", checked_in_at: null }), ZONE)).toEqual({
    tone: "red",
    heading: "This ticket was cancelled",
    body: "It was refunded or its order was cancelled, so it can't get anyone in.",
    action: "Scan the next ticket",
    autoDismissMs: null,
  });
});

test("a scan that didn't reach us offers to try again", () => {
  expect(viewFor("network", ZONE)).toEqual({
    tone: "grey",
    heading: "That scan didn't reach us",
    body: "Check your signal, then try again.",
    action: "Try again",
    autoDismissMs: null,
  });
});

test("the same value waits 3 seconds; anything else goes straight through", () => {
  const last = { value: "abc", at: 1_000 };
  expect(shouldSend("abc", last, 1_000 + 2_999)).toBe(false);
  expect(shouldSend("abc", last, 1_000 + 3_000)).toBe(true);
  expect(shouldSend("xyz", last, 1_000 + 10)).toBe(true);
  expect(shouldSend("abc", null, 0)).toBe(true);
});
