import type { AppOptions } from "./app";

/** https, or http on loopback only, reduced to its origin; "" (with a warning) for anything else. */
function storefrontOrigin(raw: string | undefined): string {
  if (!raw) return "";
  try {
    const u = new URL(raw);
    const local = ["localhost", "127.0.0.1", "[::1]"].includes(u.hostname);
    if (u.protocol === "https:" || (u.protocol === "http:" && local)) return u.origin;
  } catch {
    // falls through to the warning
  }
  console.warn("TS_STOREFRONT_URL is not an https URL (http only on localhost); ignoring it.");
  return "";
}

/**
 * Server settings from the environment. Safe by default: Secure cookies
 * unless TS_DASHBOARD_DEV=1 (plain-http local development), and listening on
 * loopback only, behind the reverse proxy, unless TS_DASHBOARD_HOST says otherwise.
 */
export function serverOptions(env: Record<string, string | undefined>) {
  return {
    hostname: env.TS_DASHBOARD_HOST || "127.0.0.1",
    port: Number(env.TS_DASHBOARD_PORT ?? 3002),
    app: {
      apiUrl: env.TS_API_URL ?? "http://127.0.0.1:3000/v1",
      staticDir: env.TS_DASHBOARD_STATIC ?? "./dist",
      secureCookies: env.TS_DASHBOARD_DEV !== "1",
      turnstileSiteKey: env.TS_TURNSTILE_SITEKEY ?? "",
      storefrontUrl: storefrontOrigin(env.TS_STOREFRONT_URL),
    } satisfies AppOptions,
  };
}
