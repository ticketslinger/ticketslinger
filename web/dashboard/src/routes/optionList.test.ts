import { expect, test } from "bun:test";
import {
  addOption,
  duplicateAt,
  foldOption,
  moveOption,
  removeOption,
  renameOption,
} from "./optionList";

test("adding, renaming and removing options returns new lists", () => {
  const a = ["Red", "Blue"];
  expect(addOption(a)).toEqual(["Red", "Blue", ""]);
  expect(addOption(a, "Green")).toEqual(["Red", "Blue", "Green"]);
  expect(renameOption(a, 1, "Navy")).toEqual(["Red", "Navy"]);
  expect(removeOption(a, 0)).toEqual(["Blue"]);
  expect(a).toEqual(["Red", "Blue"]);
});

test("moving an option stops at the ends", () => {
  const a = ["a", "b", "c"];
  expect(moveOption(a, 1, -1)).toEqual(["b", "a", "c"]);
  expect(moveOption(a, 1, 1)).toEqual(["a", "c", "b"]);
  expect(moveOption(a, 0, -1)).toEqual(a);
  expect(moveOption(a, 2, 1)).toEqual(a);
});

test("duplicates ignore capitals and spaces, the way the API does", () => {
  expect(duplicateAt(["Red", "Blue"])).toBe(-1);
  expect(duplicateAt(["Red", "Blue", " red "])).toBe(2);
  expect(duplicateAt(["", ""])).toBe(-1);
  // long s and s, final and medial sigma: one key each
  expect(duplicateAt(["ſ", "S"])).toBe(1);
  expect(duplicateAt(["ς", "Σ"])).toBe(1);
  expect(duplicateAt(["σ", "ς"])).toBe(1);
  // sharp s has no one-letter capital, so the API keeps it apart from SS
  expect(duplicateAt(["ß", "SS"])).toBe(-1);
  expect(foldOption("Ǆ")).toBe(foldOption("ǆ"));
});
