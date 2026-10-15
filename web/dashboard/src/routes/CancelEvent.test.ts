import { expect, test } from "bun:test";
import { canCancel } from "./CancelEvent";

test("cancelling needs the event's name, spaces aside", () => {
  expect(canCancel(" Spring Gala ", "Spring Gala")).toBe(true);
  expect(canCancel("spring gala", "Spring Gala")).toBe(false);
  expect(canCancel("", "Spring Gala")).toBe(false);
});
