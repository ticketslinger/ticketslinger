import { expect, test } from "bun:test";
import { callbackOutcome } from "./StripeCallback";

test("a code and state finish the link", () => {
  expect(callbackOutcome("?code=ac_123&state=abc_-9")).toEqual({
    kind: "complete",
    state: "abc_-9",
    code: "ac_123",
  });
});

test("saying no on Stripe's page is a decline, not an error", () => {
  expect(
    callbackOutcome("?error=access_denied&error_description=The+user+denied&state=abc"),
  ).toEqual({ kind: "declined" });
});

test("anything else can't be finished", () => {
  for (const search of [
    "",
    "?code=ac_1",
    "?state=abc",
    "?error=invalid_scope&state=abc",
    "?code=&state=abc",
  ]) {
    expect(callbackOutcome(search)).toEqual({ kind: "bad" });
  }
  expect(callbackOutcome("?code=ac_1&state=abc&error=server_error")).toEqual({ kind: "bad" });
});
