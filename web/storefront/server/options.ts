import type { StorefrontOptions } from "./app";

/** Server settings from the environment: loopback only, behind the reverse proxy, unless told otherwise. */
export function serverOptions(env: Record<string, string | undefined>) {
  return {
    hostname: env.TS_STOREFRONT_HOST || "127.0.0.1",
    port: Number(env.TS_STOREFRONT_PORT ?? 3001),
    app: {
      apiUrl: env.TS_API_URL ?? "http://127.0.0.1:3000/v1",
      staticDir: env.TS_STOREFRONT_STATIC ?? "./dist",
      turnstileSiteKey: env.TS_TURNSTILE_SITEKEY ?? "",
    } satisfies StorefrontOptions,
  };
}
