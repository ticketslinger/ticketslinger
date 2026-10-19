import { expect, test } from "bun:test";
import { ApiFailure } from "../api";
import { eventTabs } from "./Events";
import { ROLE } from "./Home";
import type { Org } from "./Organizer";
import { explain, ROLE_CHOICES } from "./Team";

test("roles read as the spec words them", () => {
  expect([ROLE.owner, ROLE.admin, ROLE.checkin]).toEqual(["Owner", "Admin", "Check-in"]);
  expect(ROLE_CHOICES).toEqual({
    checkin: "Check-in: scans tickets and finds guests",
    admin: "Admin: runs events, refunds and reports",
  });
});

test("team errors carry the spec's copy exactly", () => {
  const said = (code: string) =>
    (explain(new ApiFailure(code, "server words", 404)) as Error).message;
  expect(said("no_account")).toBe(
    "There's no TicketSlinger account with that email yet. Ask them to sign up at app.ticketslinger.com, then add them here.",
  );
  expect(said("already_member")).toBe("They're already on your team.");
  expect(said("cannot_remove")).toBe(
    "Owners can't be removed here, and you can't remove yourself.",
  );
  expect(said("forbidden_role")).toBe("server words");
});

test("check-in staff get no Orders tab; editors do", () => {
  const tabs = (role: Org["role"]) =>
    eventTabs({ id: "o", name: "Acme", slug: "acme", role }).map((t) => t.label);
  expect(tabs("checkin")).not.toContain("Orders");
  expect(tabs("admin")).toContain("Orders");
  expect(tabs("owner")).toContain("Orders");
});
