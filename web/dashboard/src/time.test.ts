import { expect, test } from "bun:test";
import { formatDay, fromLocalInput, toLocalInput } from "./time";

const NY = "America/New_York";

test("shows an instant as wall-clock time in the event's zone", () => {
  expect(toLocalInput("2026-07-04T23:30:00Z", NY)).toBe("2026-07-04T19:30");
  expect(toLocalInput("2026-01-15T00:05:00Z", NY)).toBe("2026-01-14T19:05");
  expect(toLocalInput("2026-07-04T23:30:00Z", "Europe/London")).toBe("2026-07-05T00:30");
  expect(toLocalInput("2026-07-04T00:00:00Z", "UTC")).toBe("2026-07-04T00:00");
});

test("reads wall-clock time in the event's zone back as UTC", () => {
  expect(fromLocalInput("2026-07-04T19:30", NY)).toBe("2026-07-04T23:30:00.000Z");
  expect(fromLocalInput("2026-01-14T19:05", NY)).toBe("2026-01-15T00:05:00.000Z");
  expect(fromLocalInput("2026-07-05T00:30", "Europe/London")).toBe("2026-07-04T23:30:00.000Z");
  expect(fromLocalInput("2026-12-31T23:59", "Asia/Kolkata")).toBe("2026-12-31T18:29:00.000Z");
});

test("round trips on either side of both 2026 New York changes", () => {
  for (const iso of [
    "2026-03-08T06:59:00.000Z",
    "2026-03-08T07:00:00.000Z",
    "2026-11-01T05:30:00.000Z",
    "2026-11-01T07:00:00.000Z",
  ]) {
    expect(fromLocalInput(toLocalInput(iso, NY), NY)).toBe(iso);
  }
});

test("a time skipped by the spring change moves forward by the gap", () => {
  // Clocks jump from 02:00 EST to 03:00 EDT, so 02:30 never happens; it becomes 03:30.
  expect(fromLocalInput("2026-03-08T02:30", NY)).toBe("2026-03-08T07:30:00.000Z");
  expect(toLocalInput(fromLocalInput("2026-03-08T02:30", NY), NY)).toBe("2026-03-08T03:30");
});

test("a half-hour gap moves forward by half an hour", () => {
  // Lord Howe Island goes from +10:30 to +11 at 02:00 on 4 October 2026.
  const LH = "Australia/Lord_Howe";
  expect(fromLocalInput("2026-10-04T02:15", LH)).toBe("2026-10-03T15:45:00.000Z");
  expect(toLocalInput("2026-10-03T15:45:00.000Z", LH)).toBe("2026-10-04T02:45");
});

test("a time repeated by the autumn change means its first occurrence", () => {
  // 01:30 happens in EDT (05:30Z) and again in EST (06:30Z).
  expect(fromLocalInput("2026-11-01T01:30", NY)).toBe("2026-11-01T05:30:00.000Z");
});

test("formatDay keeps the calendar day whatever the browser zone", () => {
  expect(formatDay("2026-10-19")).toContain("19");
});
