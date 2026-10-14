import { expect, test } from "bun:test";
import { percentToBp } from "./EventForm";
import {
  discountText,
  emptyPromo,
  normalizeCode,
  promoBody,
  promoFieldFor,
  usesText,
} from "./Promos";

const ok = { ...emptyPromo, code: "spring", value: "10" };
const field = (d: Partial<typeof ok>) =>
  (promoBody({ ...ok, ...d }, "UTC") as { field: string }).field;

test("percent text becomes basis points", () => {
  expect(percentToBp("12.5")).toBe(1250);
  expect(percentToBp("100")).toBe(10000);
  expect(percentToBp("100.00")).toBe(10000);
  expect(percentToBp("0.01")).toBe(1);
  expect(percentToBp("100.01")).toBeNull();
  expect(percentToBp("101")).toBeNull();
  expect(percentToBp("1000")).toBeNull();
  expect(percentToBp("12.345")).toBeNull();
  expect(percentToBp("abc")).toBeNull();
  expect(percentToBp("-5")).toBeNull();
});

test("a percent promo form becomes API fields", () => {
  expect(promoBody({ ...ok, value: "12.5", maxUses: "50" }, "UTC")).toEqual({
    body: {
      code: "SPRING",
      kind: "percent",
      value: 1250,
      max_uses: 50,
      valid_from: null,
      valid_until: null,
      applies_to_all: true,
      applies_to: [],
      unlocks: [],
    },
  });
  expect(field({ value: "0" })).toBe("value");
  expect(field({ value: "" })).toBe("value");
  expect(field({ value: "100.5" })).toBe("value");
  expect(field({ value: "12.345" })).toBe("value");
  expect(field({ value: "abc" })).toBe("value");
  expect(promoBody({ ...ok, value: "100" }, "UTC")).toMatchObject({ body: { value: 10000 } });
});

test("an amount promo form is in dollars, 1 cent to $100,000", () => {
  const amount = { ...ok, kind: "amount" as const };
  expect(promoBody({ ...amount, value: "$5" }, "UTC")).toMatchObject({
    body: { kind: "amount", value: 500 },
  });
  expect(field({ ...amount, value: "0" })).toBe("value");
  expect(field({ ...amount, value: "0.00" })).toBe("value");
  expect(field({ ...amount, value: "100000.01" })).toBe("value");
  expect(promoBody({ ...amount, value: "100000" }, "UTC")).toMatchObject({
    body: { value: 10_000_000 },
  });
});

test("codes are trimmed and upper-cased, then checked", () => {
  expect(normalizeCode("  early-Bird ")).toBe("EARLY-BIRD");
  expect(promoBody({ ...ok, code: " early-Bird " }, "UTC")).toMatchObject({
    body: { code: "EARLY-BIRD" },
  });
  expect(field({ code: "ab" })).toBe("code");
  expect(field({ code: "has space" })).toBe("code");
  expect(field({ code: "x".repeat(33) })).toBe("code");
});

test("max uses is empty or a whole number of 1 or more", () => {
  expect(promoBody({ ...ok, maxUses: "" }, "UTC")).toMatchObject({ body: { max_uses: null } });
  for (const maxUses of ["0", "-1", "1.5", "abc"]) expect(field({ maxUses })).toBe("max");
});

test("the validity window is read in the event's zone", () => {
  const r = promoBody(
    { ...ok, from: "2026-07-01T09:00", until: "2026-07-02T09:00" },
    "America/New_York",
  );
  expect(r).toMatchObject({
    body: { valid_from: "2026-07-01T13:00:00.000Z", valid_until: "2026-07-02T13:00:00.000Z" },
  });
  expect(field({ from: "2026-07-02T09:00", until: "2026-07-02T09:00" })).toBe("until");
  expect(field({ from: "nope" })).toBe("from");
});

test("all tickets sends no list; a limited code needs at least one ticket", () => {
  expect(promoBody({ ...ok, all: true, types: ["a"] }, "UTC")).toMatchObject({
    body: { applies_to_all: true, applies_to: [] },
  });
  expect(promoBody({ ...ok, all: false, types: ["a"] }, "UTC")).toMatchObject({
    body: { applies_to_all: false, applies_to: ["a"] },
  });
  expect(field({ all: false, types: [] })).toBe("types");
});

test("API error codes point at fields", () => {
  expect(promoFieldFor("code_taken", "")).toBe("code");
  expect(promoFieldFor("bad_code", "")).toBe("code");
  expect(promoFieldFor("bad_discount", "")).toBe("value");
  expect(promoFieldFor("value_required_with_kind", "")).toBe("value");
  expect(promoFieldFor("bad_validity", "")).toBe("until");
  expect(promoFieldFor("applies_to_required", "")).toBe("types");
  expect(promoFieldFor("unlock_not_hidden", "")).toBe("unlocks");
  expect(promoFieldFor("missing_field", "Send “value”: the discount")).toBe("value");
  expect(promoFieldFor("whatever", "")).toBe("");
});

test("list text", () => {
  expect(discountText({ kind: "percent", value: 1250 })).toBe("12.5% off");
  expect(discountText({ kind: "amount", value: 500 })).toBe("$5.00 off");
  expect(usesText({ used_count: 3, max_uses: 50 })).toBe("3 / 50");
  expect(usesText({ used_count: 3, max_uses: null })).toBe("3 / no limit");
});

test("editing a code whose tickets are gone leaves the scope out of the patch", () => {
  const loaded = { all: false, types: [] as string[] };
  const gone = { ...ok, all: false, types: [] as string[] };
  // untouched scope: not sent, and not a reason to refuse the save
  const r = promoBody(gone, "UTC", loaded) as { body: Record<string, unknown> };
  expect(r.body).not.toHaveProperty("applies_to_all");
  expect(r.body).not.toHaveProperty("applies_to");
  // same tickets, different order: still untouched
  const two = promoBody({ ...ok, all: false, types: ["b", "a"] }, "UTC", {
    all: false,
    types: ["a", "b"],
  });
  expect((two as { body: Record<string, unknown> }).body).not.toHaveProperty("applies_to");
  // touched: sent, and the empty-list rule applies again
  expect(promoBody({ ...ok, all: true }, "UTC", loaded)).toMatchObject({
    body: { applies_to_all: true, applies_to: [] },
  });
  expect(promoBody({ ...ok, all: false, types: ["a"] }, "UTC", loaded)).toMatchObject({
    body: { applies_to_all: false, applies_to: ["a"] },
  });
  // a new code still insists
  expect(field({ all: false, types: [] })).toBe("types");
});
