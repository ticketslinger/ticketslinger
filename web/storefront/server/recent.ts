/**
 * What happened to checkout requests in the last minute, by request key. createCheckout
 * never replays a stored answer, so this is how a second tap of the same form finds out
 * what the first tap got.
 */
// ponytail: one process's memory. A double tap that lands on two storefront processes still
// makes two carts; move this to a shared store (the API) if we ever run more than one.
export class Recent<V> {
  readonly #m = new Map<string, { value: V; at: number }>();
  readonly #ttl: number;
  readonly #max: number;
  readonly #now: () => number;

  constructor(ttlMs = 60_000, max = 10_000, now = () => Date.now()) {
    this.#ttl = ttlMs;
    this.#max = max;
    this.#now = now;
  }

  get(key: string): V | undefined {
    const e = this.#m.get(key);
    if (!e) return undefined;
    if (this.#now() - e.at >= this.#ttl) {
      this.#m.delete(key);
      return undefined;
    }
    return e.value;
  }

  delete(key: string) {
    this.#m.delete(key);
  }

  set(key: string, value: V) {
    this.#m.delete(key);
    this.#m.set(key, { value, at: this.#now() });
    // Map keeps insertion order, so the first key is the oldest.
    for (const k of this.#m.keys()) {
      if (this.#m.size <= this.#max) break;
      this.#m.delete(k);
    }
  }
}
