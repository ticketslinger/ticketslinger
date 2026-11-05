import { expect, test } from "bun:test";
import { bpToPercent, FEE_HINTS, percentToBp } from "./EventForm";

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
