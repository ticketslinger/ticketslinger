import { expect, test } from "bun:test";
import { match } from "./ui";

test("matches fixed segments and captures named ones", () => {
  expect(match("/o/:slug", "/o/eves-events")).toEqual({ slug: "eves-events" });
  expect(match("/o/:slug/events/:id", "/o/a/events/123")).toEqual({ slug: "a", id: "123" });
  expect(match("/o/:slug/events/new", "/o/a/events/new")).toEqual({ slug: "a" });
  expect(match("/security", "/security")).toEqual({});
});

test("ignores one trailing slash", () => {
  expect(match("/o/:slug", "/o/a/")).toEqual({ slug: "a" });
  expect(match("/o/:slug/audit", "/o/a/audit/")).toEqual({ slug: "a" });
});

test("decodes captured segments", () => {
  expect(match("/o/:slug", "/o/caf%C3%A9")).toEqual({ slug: "café" });
});

test("rejects anything that doesn't line up", () => {
  expect(match("/o/:slug", "/o")).toBeNull();
  expect(match("/o/:slug", "/o/")).toBeNull();
  expect(match("/o/:slug", "/o/a//")).toBeNull();
  expect(match("/o/:slug", "/o/a/events")).toBeNull();
  expect(match("/o/:slug/audit", "/o/a/api-keys")).toBeNull();
  expect(match("/o/:slug", "/x/a")).toBeNull();
  expect(match("/o/:slug", "/o/%E0%A4%A")).toBeNull();
});
