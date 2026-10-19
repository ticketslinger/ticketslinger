import { expect, test } from "bun:test";
import { CANCEL_WARNING, canCancel, cancelledText } from "./CancelEvent";

test("cancelling needs the event's name, spaces aside", () => {
  expect(canCancel(" Spring Gala ", "Spring Gala")).toBe(true);
  expect(canCancel("spring gala", "Spring Gala")).toBe(false);
  expect(canCancel("", "Spring Gala")).toBe(false);
});

test("the result counts the orders we're refunding", () => {
  expect(cancelledText(2, 7800)).toBe("Cancelled. We're refunding 2 orders, $78.00 in all.");
  expect(cancelledText(1, 2600)).toBe("Cancelled. We're refunding 1 order, $26.00 in all.");
  expect(cancelledText(0, 0)).toBe("Cancelled. There was nothing left to refund.");
});

test("the warning says what happens to refunds made already", () => {
  expect(CANCEL_WARNING).toContain(
    "Everyone holding a ticket that still works gets their money back, our service fee included. Refunds you've already made stay as they are, and any that failed still need a Try again from the order.",
  );
});
