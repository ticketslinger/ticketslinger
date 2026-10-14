import { expect, test } from "bun:test";
import { slugify } from "./NewOrganizer";

test("slugify turns a name into a valid slug", () => {
  expect(slugify("The Crown & Anchor")).toBe("the-crown-anchor");
  expect(slugify("  Café Müller!! ")).toBe("cafe-muller");
  expect(slugify("--a--b--")).toBe("a-b");
  const long = slugify(`${"x".repeat(39)} yz`);
  expect(long.length).toBeLessThanOrEqual(40);
  expect(long).toMatch(/^[a-z0-9](-?[a-z0-9])*$/);
});
