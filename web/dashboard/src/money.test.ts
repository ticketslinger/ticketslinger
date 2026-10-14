import { expect, test } from "bun:test";
import { formatCents, parseDollars } from "./money";

test("dollars become cents", () => {
  expect(parseDollars("12")).toBe(1200);
  expect(parseDollars("12.5")).toBe(1250);
  expect(parseDollars("12.50")).toBe(1250);
  expect(parseDollars("$12.50")).toBe(1250);
  expect(parseDollars(" $ 12.50 ")).toBe(1250);
  expect(parseDollars("1,200.00")).toBe(120000);
  expect(parseDollars("0")).toBe(0);
  expect(parseDollars("0.07")).toBe(7);
  expect(parseDollars("19.99")).toBe(1999);
});

test("anything else is rejected", () => {
  for (const bad of [
    "",
    "-5",
    "-0.50",
    "12.345",
    "abc",
    "12a",
    "1,20",
    "12,00,000",
    "12.",
    ".5",
    "1e3",
    "$",
    "12 50",
  ]) {
    expect(parseDollars(bad)).toBeNull();
  }
});

test("cents show as dollars", () => {
  expect(formatCents(0)).toBe("$0.00");
  expect(formatCents(7)).toBe("$0.07");
  expect(formatCents(1250)).toBe("$12.50");
  expect(formatCents(120000)).toBe("$1,200.00");
  expect(formatCents(10000000)).toBe("$100,000.00");
});

test("parse and format agree", () => {
  for (const c of [0, 1, 99, 100, 1999, 120000, 10000000]) {
    expect(parseDollars(formatCents(c))).toBe(c);
  }
});
