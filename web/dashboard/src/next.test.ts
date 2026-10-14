import { expect, test } from "bun:test";
import { safeNext } from "./ui";

test("keeps same-site paths and rejects everything else", () => {
  expect(safeNext("/organizers/new")).toBe("/organizers/new");
  expect(safeNext("/e?x=1")).toBe("/e?x=1");
  for (const bad of [
    null,
    "",
    "https://evil.example",
    "//evil.example",
    "/\\evil.example",
    "javascript:alert(1)",
    "organizers",
    "\\\\evil.example",
    "/\t/evil.example",
    "/\n/evil.example",
  ]) {
    expect(safeNext(bad)).toBe("/");
  }
});
