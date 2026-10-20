import { expect, test } from "bun:test";
import { describeAction as say, staffRow, who } from "./Audit";

test("audit actions read as plain words", () => {
  expect(say("event.created")).toBe("Event created");
  expect(say("ticket_type.updated")).toBe("Ticket type changed");
  expect(say("ticket_type.reordered")).toBe("Ticket type reordered");
  expect(say("quota.deleted")).toBe("Capacity deleted");
  expect(say("api_key.revoked")).toBe("API key revoked");
  expect(say("order.cancelled_late")).toBe("Order cancelled (refund queued)");
  expect(say("organizer.stripe_linked")).toBe("Organizer linked a Stripe account");
  expect(say("organizer.stripe_disconnected")).toBe("Organizer disconnected from Stripe");
  expect(say("refund.succeeded_late")).toBe("Refund went through after all");
  expect(say("refund.fee_stuck")).toBe("Refund stuck on our fee");
  expect(say("refund.retried")).toBe("Refund sent again");
  expect(say("order.email_resent")).toBe("Order ticket email sent again");
  expect(say("order.refunded")).toBe("Order refunded");
  expect(say("event.cancelled")).toBe("Event cancelled");
  expect(say("order.cancelled")).toBe("Order cancelled");
  expect(say("member.added")).toBe("Team member added");
  expect(say("member.removed")).toBe("Team member removed");
  expect(say("seat.teleported")).toBe("seat.teleported");
});

test("the Who cell names members and keys, and falls back kindly", () => {
  const me = "u1";
  expect(who({ actor_type: "user", actor_user_id: "u1", actor_name: "Olive" }, me)).toBe("You");
  expect(who({ actor_type: "user", actor_user_id: "u2", actor_name: "Sam" }, me)).toBe("Sam");
  expect(who({ actor_type: "user", actor_user_id: "u3", actor_name: null }, me)).toBe(
    "Someone on your team",
  );
  expect(who({ actor_type: "user", actor_name: null }, me)).toBe("Someone on your team");
  expect(who({ actor_type: "api_key", actor_api_key_id: "k1", actor_name: "CI" }, me)).toBe(
    "API key: CI",
  );
  expect(who({ actor_type: "api_key", actor_api_key_id: "k1" }, me)).toBe("An API key");
  expect(who({ actor_type: "system", actor_name: null }, me)).toBe("Automatic");
});

test("support's rows say what support did, and the Who cell says it was support", () => {
  const row = (action: string) => staffRow({ action, actor_name: "Sam" });
  expect(row("staff.suspended")).toBe("Suspended your organizer, so your events went off sale");
  expect(row("staff.lifted")).toBe("Lifted your organizer's suspension");
  expect(row("staff.refund_requested")).toBe("Started refunding every order of an event");
  expect(row("staff.session_started")).toBe("Opened your organizer in support mode");
  expect(row("staff.session_ended")).toBe("TicketSlinger support (Sam) ended support mode");
  expect(say("event.refunded_by_support")).toBe(
    "Every order of an event refunded in full, our fee too",
  );
  const me = "u1";
  expect(who({ actor_type: "staff", actor_user_id: "s1", actor_name: "Sam" }, me)).toBe(
    "TicketSlinger support (Sam)",
  );
  expect(who({ actor_type: "staff", actor_user_id: "s1", actor_name: null }, me)).toBe(
    "TicketSlinger support",
  );
  expect(
    who({ actor_type: "system", actor_name: null, action: "event.refunded_by_support" }, me),
  ).toBe("TicketSlinger support");
});
