import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import { ApiFailure } from "../api";
import {
  bpToPercent,
  EventForm,
  FEE_HINTS,
  NAMES_LOCK_ERROR,
  NAMES_LOCK_HINT,
  namesLockFailure,
  namesLockValue,
  percentToBp,
} from "./EventForm";

test("basis points show as a percent with up to two decimals", () => {
  expect(bpToPercent(0)).toBe("0");
  expect(bpToPercent(825)).toBe("8.25");
  expect(bpToPercent(850)).toBe("8.5");
  expect(bpToPercent(800)).toBe("8");
  expect(bpToPercent(7)).toBe("0.07");
  expect(bpToPercent(10000)).toBe("100");
});

test("a typed percent becomes exact basis points", () => {
  expect(percentToBp("8.25")).toBe(825);
  expect(percentToBp("8.5")).toBe(850);
  expect(percentToBp(" 8 ")).toBe(800);
  expect(percentToBp("0.07")).toBe(7);
  expect(percentToBp("0.29")).toBe(29); // 0.29 * 100 is 28.999… in floats
  expect(percentToBp("1.13")).toBe(113);
  expect(percentToBp("100")).toBe(10000);
  expect(percentToBp(".5")).toBe(50);
  expect(percentToBp("")).toBe(0);
});

test("anything else is refused", () => {
  for (const bad of ["8.255", "100.01", "-1", "abc", "8,25", "1e2", "8.", "101"]) {
    expect(percentToBp(bad)).toBeNull();
  }
});

test("the fee-mode hints say what buyers see and what the organizer keeps", () => {
  expect(FEE_HINTS.pass).toBe(
    "Buyers see one price with the fee already in it. You get your full ticket price.",
  );
  expect(FEE_HINTS.absorb).toBe("It comes out of your ticket price, so buyers pay no fee.");
});

test("the fee-mode radios render their hints", () => {
  const org = { slug: "x", role: "owner" } as Parameters<typeof EventForm>[0]["org"];
  const html = renderToString(createElement(EventForm, { org }));
  expect(html).toContain(FEE_HINTS.pass);
  expect(html).toContain(FEE_HINTS.absorb);
});

test("clearing the names lock sends null; a time is read in the event's zone", () => {
  expect(namesLockValue("", "Europe/London")).toBeNull();
  expect(namesLockValue("2026-07-01T12:00", "Europe/London")).toBe("2026-07-01T11:00:00.000Z");
});

test("the server's names_lock_after_start shows the spec sentence on that field", () => {
  expect(NAMES_LOCK_ERROR).toBe("Pick a time before the event starts.");
  const e = new ApiFailure("names_lock_after_start", "server words", 422, "names_locked_at");
  expect(namesLockFailure(e).message).toBe(NAMES_LOCK_ERROR);
  const other = new ApiFailure("rate_limited", "slow down", 429);
  expect(namesLockFailure(other)).toBe(other);
});

test("the names lock field renders with its hint", () => {
  const org = { slug: "x", role: "owner" } as Parameters<typeof EventForm>[0]["org"];
  const html = renderToString(createElement(EventForm, { org }));
  expect(html).toContain("Buyers can change names until");
  expect(html).toContain(NAMES_LOCK_HINT);
});

test("the names lock error is an always-present region the input points at", () => {
  const org = { slug: "x", role: "owner" } as Parameters<typeof EventForm>[0]["org"];
  const html = renderToString(createElement(EventForm, { org }));
  expect(html).toContain('id="event-names-error"');
  expect(html).toContain('aria-describedby="event-names-hint event-names-error"');
});
