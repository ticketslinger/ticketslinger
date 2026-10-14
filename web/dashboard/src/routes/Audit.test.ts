import { expect, test } from "bun:test";
import { describeAction as say } from "./Audit";

test("audit actions read as plain words", () => {
  expect(say("event.created")).toBe("Event created");
  expect(say("ticket_type.updated")).toBe("Ticket type changed");
  expect(say("quota.deleted")).toBe("Capacity deleted");
  expect(say("api_key.revoked")).toBe("API key revoked");
  expect(say("seat.teleported")).toBe("seat.teleported");
});
