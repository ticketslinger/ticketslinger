import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import { ApiFailure } from "../api";
import { eventTabs } from "./Events";
import { ROLE } from "./Home";
import type { Org } from "./Organizer";
import { explain, ROLE_CHOICES, removeWarning, Team } from "./Team";

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
    eventTabs({ id: "o", name: "Acme", slug: "acme", role, suspended: false }).map((t) => t.label);
  expect(tabs("checkin")).not.toContain("Orders");
  expect(tabs("checkin")).not.toContain("Report");
  expect(tabs("admin")).toContain("Report");
  expect(tabs("admin")).toContain("Orders");
  expect(tabs("owner")).toContain("Orders");
  for (const role of ["owner", "admin", "checkin"] as const)
    expect(tabs(role)).toContain("Check-in");
});

test("the remove confirm says their keys stop working too", () => {
  expect(removeWarning("Acme")).toBe(
    "They lose access to Acme straight away, and any API keys they made here stop working.",
  );
});

test("owners and admins get the order email setting; check-in staff don't", () => {
  const html = (role: Org["role"]) =>
    renderToString(
      createElement(Team, { org: { id: "o", name: "Acme", slug: "acme", role, suspended: false } }),
    );
  expect(html("owner")).toContain("Email me about each new online order");
  expect(html("owner")).toContain("At most 50 a day. Comp and manual orders don&#x27;t send one.");
  expect(html("admin")).toContain("Email me about each new online order");
  expect(html("checkin")).not.toContain("Email me about each new online order");
});
