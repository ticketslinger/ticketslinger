import { expect, test } from "bun:test";
import { publicEventUrl, publicLinkState } from "./publicLink";

test("builds the storefront address from organizer and event slugs", () => {
  expect(publicEventUrl("https://tickets.example.com", "demo", "test-event")).toBe(
    "https://tickets.example.com/demo/test-event",
  );
  expect(publicEventUrl("https://tickets.example.com//", "a b", "ö/x")).toBe(
    "https://tickets.example.com/a%20b/%C3%B6%2Fx",
  );
  expect(publicEventUrl("", "demo", "test-event")).toBe("");
});

test("link shows when live, a note when draft, nothing otherwise", () => {
  const url = "https://t.example.com/o/e";
  expect(publicLinkState("live", url)).toBe("link");
  expect(publicLinkState("draft", url)).toBe("draft");
  expect(publicLinkState("draft", "")).toBe("draft");
  expect(publicLinkState("live", "")).toBe("none");
  expect(publicLinkState("ended", url)).toBe("none");
  expect(publicLinkState("cancelled", url)).toBe("none");
});
