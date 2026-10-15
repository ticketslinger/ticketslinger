import { expect, test } from "bun:test";
import { ApiFailure } from "./api";
import { nextKey } from "./idempotency";

const fail = (status: number) => new ApiFailure("x", "m", status);

test("a key survives only the failures the API never stored", () => {
  expect(nextKey("k")).toBeNull();
  for (const s of [0, 429, 500, 503]) expect(nextKey("k", fail(s))).toBe("k");
  for (const s of [400, 403, 409, 422]) expect(nextKey("k", fail(s))).toBeNull();
  expect(nextKey("k", new TypeError("boom"))).toBe("k");
});
