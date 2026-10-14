import { expect, test } from "bun:test";
import { describeAction as say, who } from "./Audit";

test("audit actions read as plain words", () => {
  expect(say("event.created")).toBe("Event created");
  expect(say("ticket_type.updated")).toBe("Ticket type changed");
  expect(say("ticket_type.reordered")).toBe("Ticket type reordered");
  expect(say("quota.deleted")).toBe("Capacity deleted");
  expect(say("api_key.revoked")).toBe("API key revoked");
  expect(say("order.cancelled_late")).toBe("Order cancelled and refunded");
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
