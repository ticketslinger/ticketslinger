import { expect, test } from "bun:test";
import { emptyTicket, needsCapacity, quotaBody, ticketBody } from "./Tickets";

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

test("grouped quota sizes must be grouped properly", () => {
  for (const size of ["1,2,3", "12,34", "1,23", ",100"])
    expect(quotaBody({ name: "A", size, types: [] })).toMatchObject({ field: "size" });
  expect(quotaBody({ name: "A", size: "1000000", types: [] })).toMatchObject({
    body: { size: 1000000 },
  });
  expect(quotaBody({ name: "A", size: "1000001", types: [] })).toMatchObject({ field: "size" });
});

test("a sales time in a clock-change gap moves forward, as the event form does", () => {
  const r = ticketBody(
    { ...ok, start: "2026-03-08T02:30", end: "2026-03-08T03:45" },
    "America/New_York",
  );
  expect(r).toMatchObject({
    body: { sales_start: "2026-03-08T07:30:00.000Z", sales_end: "2026-03-08T07:45:00.000Z" },
  });
});

test("only a live event insists on a capacity", () => {
  expect(needsCapacity("live")).toBe(true);
  for (const s of ["draft", "ended", "cancelled"] as const) expect(needsCapacity(s)).toBe(false);
});
