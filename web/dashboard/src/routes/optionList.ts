// Pure helpers for the option list of a select question, and for moving
// questions up and down.

const one = (s: string) => ([...s].length === 1 ? s : null);

/**
 * The key the API compares options by: upper case, then lower case, one
 * letter at a time (Go's simple case mapping). That puts ſ with s and ς with
 * σ, but leaves ß alone, where JavaScript's own toUpperCase would make it SS.
 * The API has the last word; this only warns early.
 */
export const foldOption = (s: string) =>
  [...s]
    .map((c) => one(c.toUpperCase()) ?? c)
    .map((c) => one(c.toLowerCase()) ?? c)
    .join("");

export const addOption = (list: string[], text = "") => [...list, text];
export const removeOption = (list: string[], i: number) => list.filter((_, j) => j !== i);
export const renameOption = (list: string[], i: number, text: string) =>
  list.map((o, j) => (j === i ? text : o));

export function moveOption(list: string[], i: number, dir: -1 | 1) {
  const to = i + dir;
  if (to < 0 || to >= list.length) return list;
  const out = [...list];
  [out[i], out[to]] = [out[to] as string, out[i] as string];
  return out;
}

/** The first option that repeats an earlier one, or -1. Blank rows don't count. */
export function duplicateAt(list: string[]): number {
  const seen = new Set<string>();
  for (const [i, o] of list.entries()) {
    const key = foldOption(o.trim());
    if (!key) continue;
    if (seen.has(key)) return i;
    seen.add(key);
  }
  return -1;
}

/**
 * Moving one question changes only its own position, so a step up or down
 * means trading positions with its neighbour: two patches, or null at the ends.
 */
export function swapPositions(
  list: { id: string; position: number }[],
  i: number,
  dir: -1 | 1,
): { id: string; position: number }[] | null {
  const a = list[i];
  const b = list[i + dir];
  return a && b
    ? [
        { id: a.id, position: b.position },
        { id: b.id, position: a.position },
      ]
    : null;
}
