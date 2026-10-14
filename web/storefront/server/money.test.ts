import { expect, test } from "bun:test";
import { formatCents } from "./money";

test("formatCents", () => {
  expect(formatCents(0)).toBe("$0.00");
  expect(formatCents(5)).toBe("$0.05");
  expect(formatCents(2500)).toBe("$25.00");
  expect(formatCents(123456)).toBe("$1,234.56");
  expect(formatCents(120000)).toBe("$1,200.00");
});
