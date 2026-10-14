/**
 * Checkouts made in the last minute, by idempotency key. createCheckout never replays a
 * stored answer, so this is how a second tap of the same form finds the first tap's cart.
 */
// ponytail: one process's memory. A double tap that lands on two storefront processes still
// makes two carts; move this to a shared store (the API) if we ever run more than one.
export class Recent {
  readonly #m = new Map<string, { token: string; at: number }>();
  readonly #ttl: number;
  readonly #max: number;
  readonly #now: () => number;

  constructor(ttlMs = 60_000, max = 10_000, now = () => Date.now()) {
    this.#ttl = ttlMs;
    this.#max = max;
    this.#now = now;
  }

  get(key: string): string | undefined {
    const e = this.#m.get(key);
    if (!e) return undefined;
    if (this.#now() - e.at >= this.#ttl) {
      this.#m.delete(key);
      return undefined;
    }
    return e.token;
  }

  set(key: string, token: string) {
    this.#m.delete(key);
    this.#m.set(key, { token, at: this.#now() });
    // Map keeps insertion order, so the first key is the oldest.
    for (const k of this.#m.keys()) {
      if (this.#m.size <= this.#max) break;
      this.#m.delete(k);
    }
  }
}
