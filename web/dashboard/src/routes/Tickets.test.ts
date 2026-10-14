import { expect, test } from "bun:test";
import { emptyTicket, fieldFor, quotaBody, ticketBody } from "./Tickets";

const ok = { ...emptyTicket, name: "General", price: "25" };

test("a ticket form becomes API fields", () => {
  const r = ticketBody(
    { ...ok, price: "$12.50", start: "2026-07-01T09:00", tax: "8.25", includeTax: "yes" },
    "America/New_York",
  );
  expect(r).toMatchObject({
    body: {
      name: "General",
      price_cents: 1250,
      min_per_order: 1,
      max_per_order: 10,
      sales_start: "2026-07-01T13:00:00.000Z",
      sales_end: null,
      tax_rate_bp: 825,
      prices_include_tax: true,
    },
  });
  expect(ticketBody(ok, "UTC")).toMatchObject({
    body: { tax_rate_bp: null, prices_include_tax: null },
  });
});

test("a bad ticket form names the field", () => {
  const field = (d: Partial<typeof ok>) =>
    (ticketBody({ ...ok, ...d }, "UTC") as { field: string }).field;
  expect(field({ name: " " })).toBe("name");
  expect(field({ price: "-1" })).toBe("price");
  expect(field({ price: "100000.01" })).toBe("price");
  expect(field({ min: "0" })).toBe("min");
  expect(field({ min: "5", max: "2" })).toBe("max");
  expect(field({ max: "101" })).toBe("max");
  expect(field({ start: "2026-07-02T09:00", end: "2026-07-01T09:00" })).toBe("end");
  expect(field({ tax: "101" })).toBe("tax");
});

test("a capacity form", () => {
  expect(quotaBody({ name: "Floor", size: "1,200", types: ["a"] })).toEqual({
    body: { name: "Floor", size: 1200, ticket_type_ids: ["a"] },
  });
  expect(quotaBody({ name: "Floor", size: "0", types: [] })).toMatchObject({ body: { size: 0 } });
  expect(quotaBody({ name: "Floor", size: "-3", types: [] })).toMatchObject({ field: "size" });
  expect(quotaBody({ name: "", size: "3", types: [] })).toMatchObject({ field: "name" });
});

test("API error codes point at fields", () => {
  expect(fieldFor("bad_price", "")).toBe("price");
  expect(fieldFor("missing_field", "Send “price_cents”: the price")).toBe("price");
  expect(fieldFor("missing_field", "Send “size”: how many")).toBe("size");
  expect(fieldFor("live_needs_capacity", "")).toBe("choices");
  expect(fieldFor("ticket_type_exists", "")).toBe("");
});
