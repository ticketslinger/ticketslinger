import { ApiFailure } from "./api";

/**
 * The key to use after a money-moving call that failed with `failure` (or
 * succeeded, when it's undefined). The API stores every answer except 5xx and
 * 429, so only a network error, 5xx or 429 may retry under the same key; any
 * other answer would just replay, so the next press gets a fresh one.
 */
export function nextKey(current: string | null, failure?: unknown): string | null {
  if (!(failure instanceof ApiFailure)) return failure === undefined ? null : current;
  const s = failure.status;
  return s === 0 || s === 429 || s >= 500 ? current : null;
}

/** Runs a money-moving call under the press's key, keeping or dropping the key by `nextKey`. */
export async function withKey<T>(
  ref: { current: string | null },
  call: (key: string) => Promise<T>,
): Promise<T> {
  ref.current ??= crypto.randomUUID();
  const key = ref.current;
  try {
    const r = await call(key);
    ref.current = nextKey(key);
    return r;
  } catch (e) {
    ref.current = nextKey(key, e);
    throw e;
  }
}
