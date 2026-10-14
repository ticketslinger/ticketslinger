// Links the storefront builds from data it didn't write: shared by the server and the island,
// so both check codes and tokens the same way.

// The API's checkout and order-link tokens are 32 random bytes in base64url; this is looser on length only.
export const TOKEN = /^[A-Za-z0-9_-]{16,128}$/;
// The API's order codes: TS- and five Crockford base32 characters.
export const ORDER = /^TS-[0-9A-HJKMNP-TV-Z]{5}$/;

/**
 * Our own path to an order, rebuilt from the link's code and token. The link's path is never
 * reused, so a host can't ride in on it (//evil.example/...). code, when given, must match.
 */
export function orderPath(url: string, code?: string): string | undefined {
  let u: URL;
  try {
    u = new URL(url, "http://storefront");
  } catch {
    return undefined;
  }
  const fromPath = /\/orders\/([^/]+)$/.exec(u.pathname)?.[1] ?? "";
  const c = code ?? fromPath;
  const t = u.searchParams.get("t") ?? "";
  if (!ORDER.test(c) || c !== fromPath || !TOKEN.test(t)) return undefined;
  return `/orders/${c}?t=${encodeURIComponent(t)}`;
}

/** The event page: each slug encoded, so neither can add a scheme or a host. Both are needed. */
export function eventPath(d: { organizer: string; event_slug: string }): string | undefined {
  if (!d.organizer || !d.event_slug) return undefined;
  return `/${encodeURIComponent(d.organizer)}/${encodeURIComponent(d.event_slug)}`;
}
