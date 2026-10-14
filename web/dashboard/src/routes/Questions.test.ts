import { expect, test } from "bun:test";
import { emptyQuestion, questionBody } from "./Questions";

const ok = { ...emptyQuestion, label: "Dietary needs?" };
const field = (d: Partial<typeof ok>) => (questionBody({ ...ok, ...d }) as { field: string }).field;

test("a text question has no options, even if some were typed before switching", () => {
  expect(questionBody({ ...ok, kind: "text", options: ["x", "y"] })).toEqual({
    body: {
      label: "Dietary needs?",
      kind: "text",
      options: [],
      required: false,
      scope: "order",
      applies_to_all: true,
      applies_to: [],
    },
  });
});

test("a select needs 2 to 30 different options", () => {
  const select = { kind: "select" as const };
  expect(questionBody({ ...ok, ...select, options: [" Veg ", "Vegan"] })).toMatchObject({
    body: { options: ["Veg", "Vegan"] },
  });
  expect(field({ ...select, options: ["Veg"] })).toBe("options");
  expect(field({ ...select, options: ["Veg", ""] })).toBe("option-1");
  expect(field({ ...select, options: ["Veg", "veg"] })).toBe("option-1");
  expect(field({ ...select, options: ["a", "b", "x".repeat(121)] })).toBe("option-2");
  const many = Array.from({ length: 31 }, (_, i) => `o${i}`);
  expect(field({ ...select, options: many })).toBe("options");
  expect(questionBody({ ...ok, ...select, options: many.slice(0, 30) })).toHaveProperty("body");
});

test("label, and which tickets", () => {
  expect(field({ label: "  " })).toBe("label");
  expect(field({ label: "x".repeat(201) })).toBe("label");
  expect(field({ all: false, types: [] })).toBe("types");
  expect(
    questionBody({ ...ok, all: false, types: ["a"], scope: "ticket", required: true }),
  ).toMatchObject({
    body: { applies_to_all: false, applies_to: ["a"], scope: "ticket", required: true },
  });
});
