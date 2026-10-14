import type { AppOptions } from "./app";

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
    } satisfies AppOptions,
  };
}
