import { expect, test } from "bun:test";
import { Recent } from "./recent";

test("entries expire after the TTL", () => {
  let now = 0;
  const r = new Recent(60_000, 10, () => now);
  r.set("k", "tok");
  now = 59_999;
  expect(r.get("k")).toBe("tok");
  now = 60_000;
  expect(r.get("k")).toBeUndefined();
});

test("the oldest entry goes first when full", () => {
  const r = new Recent(60_000, 2);
  r.set("a", "1");
  r.set("b", "2");
  r.set("c", "3");
  expect(r.get("a")).toBeUndefined();
  expect(r.get("b")).toBe("2");
  expect(r.get("c")).toBe("3");
});
