import { timingSafeEqual } from "node:crypto";
import { type Context, Hono } from "hono";
import { bodyLimit } from "hono/body-limit";
import { serveStatic } from "hono/bun";
import { deleteCookie, getCookie, setCookie } from "hono/cookie";

export type AppOptions = {
  apiUrl: string;
  staticDir: string;
  secureCookies: boolean;
  /** Cloudflare Turnstile site key; "" turns the widget off. */
  turnstileSiteKey?: string;
  /** Address of the TCP peer. Defaults to Bun's server.requestIP; tests inject their own. */
  peerIp?: (c: Context) => string | undefined;
};

const MAX_BODY = 1024 * 1024;
const THIRTY_DAYS = 30 * 24 * 60 * 60;

// Only these client headers reach the API (plus authorization and x-real-ip, set by us).
const ALLOW_REQ = new Set([
  "content-type",
  "content-length",
  "accept",
  "accept-language",
  "if-none-match",
  "if-modified-since",
  "idempotency-key",
  "user-agent",
]);
const DROP_RES = new Set([
  "set-cookie",
  "content-encoding",
  "content-length",
  "transfer-encoding",
  "connection",
]);

// 'wasm-unsafe-eval' lets the scanner compile its QR decoder (our own /assets/*.wasm)
// where the browser has no BarcodeDetector. It allows WebAssembly only, not eval().
const BASE = "default-src 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'";
const CSP = `${BASE}; script-src 'self' 'wasm-unsafe-eval'`;
// The HTML shell also loads and frames Turnstile when it's on.
const TURNSTILE = "https://challenges.cloudflare.com";
const SHELL_CSP = `${BASE}; script-src 'self' 'wasm-unsafe-eval' ${TURNSTILE}; frame-src ${TURNSTILE}; connect-src 'self' ${TURNSTILE}`;

const err = (c: Context, status: 400 | 403 | 404 | 413 | 502, code: string, message: string) =>
  c.json({ error: { code, message } }, status);

const isLoopback = (ip: string) => /^127\./.test(ip) || ip === "::1" || /^::ffff:127\./i.test(ip);

const safeEqual = (a: string, b: string) => {
  const x = Buffer.from(a);
  const y = Buffer.from(b);
  return x.length > 0 && x.length === y.length && timingSafeEqual(x, y);
};

// API routes that answer with a session_token. Sessions are only made through
// /session/*, which moves the token into an HttpOnly cookie; through the
// generic proxy page script would see it. Matched on the decoded path with
// repeated and trailing slashes folded, as the API's router would see it.
const SESSION_ROUTES = /^\/api\/auth\/(login|signup|passkeys\/login\/.+)$/i;
const createsSession = (pathname: string) =>
  SESSION_ROUTES.test(decodeURIComponent(pathname).replace(/\/+/g, "/").replace(/\/$/, ""));

function hasDotSegment(path: string, only: (s: string) => boolean): boolean {
  let decoded: string;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    return true;
  }
  return decoded.split(/[/\\]/).some(only);
}

export function createApp(opts: AppOptions): Hono {
  const app = new Hono();
  const apiUrl = opts.apiUrl.replace(/\/+$/, "");
  const siteKey = opts.turnstileSiteKey ?? "";
  const peerIp =
    opts.peerIp ??
    ((c: Context) =>
      (c.env as { server?: Bun.Server<unknown> } | undefined)?.server?.requestIP(c.req.raw)
        ?.address);

  // __Host- prefix blocks cookie tossing from sibling subdomains; browsers reject it without Secure.
  const SESSION = opts.secureCookies ? "__Host-ts_session" : "ts_session";
  const CSRF = opts.secureCookies ? "__Host-ts_csrf" : "ts_csrf";
  const cookieBase = { path: "/", secure: opts.secureCookies } as const;

  // Client IP is X-Real-IP only when the peer is our own reverse proxy (loopback).
  function clientIp(c: Context): string | undefined {
    const peer = peerIp(c);
    if (peer && isLoopback(peer)) return c.req.header("x-real-ip")?.trim() || peer;
    return peer;
  }

  function upstream(c: Context, url: string, withBody: boolean): Promise<Response> {
    const headers = new Headers();
    for (const [k, v] of c.req.raw.headers) if (ALLOW_REQ.has(k)) headers.set(k, v);
    const ip = clientIp(c);
    if (ip) headers.set("x-real-ip", ip);
    const token = getCookie(c, SESSION);
    if (token) headers.set("authorization", `Bearer ${token}`);
    const init: RequestInit & { duplex?: "half" } = {
      method: c.req.method,
      headers,
      redirect: "manual",
    };
    if (withBody && c.req.raw.body) {
      init.body = c.req.raw.body;
      init.duplex = "half";
    }
    return fetch(url, init);
  }

  // Passes an API response back, minus hop-by-hop headers. A JSON object's
  // top-level session_token is dropped, so no route can hand one to page script.
  async function relay(res: Response): Promise<Response> {
    const headers = new Headers();
    for (const [k, v] of res.headers) if (!DROP_RES.has(k)) headers.set(k, v);
    let body: BodyInit | null = res.body;
    if (/^application\/json\b/i.test(res.headers.get("content-type") ?? "")) {
      body = await res.text();
      try {
        const v = JSON.parse(body);
        if (v && typeof v === "object" && !Array.isArray(v) && "session_token" in v) {
          delete v.session_token;
          body = JSON.stringify(v);
        }
      } catch {
        // Not JSON after all: pass it on as it came.
      }
    }
    return new Response(body, { status: res.status, headers });
  }

  // Security headers on every response, including errors.
  app.use(async (c, next) => {
    await next();
    const h = c.res.headers;
    h.set("X-Content-Type-Options", "nosniff");
    h.set("Referrer-Policy", "strict-origin-when-cross-origin");
    h.set("X-Frame-Options", "DENY");
    const shell = siteKey && /^text\/html\b/i.test(h.get("content-type") ?? "");
    h.set("Content-Security-Policy", shell ? SHELL_CSP : CSP);
  });

  app.use(
    bodyLimit({
      maxSize: MAX_BODY,
      onError: (c) => err(c, 413, "too_large", "Request body is over 1 MiB."),
    }),
  );

  // CSRF: double-submit cookie.
  app.use(async (c, next) => {
    const m = c.req.method;
    const cookie = getCookie(c, CSRF) ?? "";
    if (m !== "GET" && m !== "HEAD") {
      if (!safeEqual(c.req.header("x-csrf-token") ?? "", cookie)) {
        return err(c, 403, "csrf", "Missing or invalid CSRF token.");
      }
    }
    await next();
    if (m === "GET" && !cookie) {
      setCookie(
        c,
        CSRF,
        Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString("base64url"),
        {
          ...cookieBase,
          httpOnly: false,
          sameSite: "Strict",
        },
      );
    }
  });

  async function login(c: Context, path: string) {
    let res: Response;
    try {
      res = await upstream(c, `${apiUrl}${path}`, true);
    } catch {
      return err(c, 502, "bad_gateway", "The API is unreachable.");
    }
    if (!res.ok) return relay(res);
    const { session_token, ...rest } = (await res.json()) as { session_token?: string };
    if (!session_token) return err(c, 502, "bad_gateway", "The API returned no session.");
    setCookie(c, SESSION, session_token, {
      ...cookieBase,
      httpOnly: true,
      sameSite: "Lax",
      maxAge: THIRTY_DAYS,
    });
    return c.json(rest, res.status as 200 | 201);
  }

  app.get("/config.json", (c) => c.json({ turnstileSiteKey: siteKey }));

  app.post("/session/login", (c) => login(c, "/auth/login"));
  app.post("/session/signup", (c) => login(c, "/auth/signup"));
  app.post("/session/passkey/:challengeId", (c) =>
    login(c, `/auth/passkeys/login/${encodeURIComponent(c.req.param("challengeId"))}`),
  );

  app.post("/session/logout", async (c) => {
    let res: Response;
    try {
      res = await upstream(c, `${apiUrl}/auth/logout`, false);
    } catch {
      res = new Response(null, { status: 204 });
    }
    c.res = await relay(res);
    deleteCookie(c, SESSION, cookieBase);
    return c.res;
  });

  app.all("/api/*", async (c) => {
    const { pathname, search } = new URL(c.req.url);
    if (!pathname.startsWith("/api/")) return err(c, 404, "not_found", "Not found.");
    if (hasDotSegment(pathname, (s) => s === "." || s === "..")) {
      return err(c, 400, "bad_path", "Invalid path.");
    }
    if (createsSession(pathname)) return err(c, 404, "not_found", "Not found.");
    try {
      return relay(await upstream(c, `${apiUrl}${pathname.slice(4)}${search}`, true));
    } catch {
      return err(c, 502, "bad_gateway", "The API is unreachable.");
    }
  });

  // Static SPA. Dotfiles are never served; missing files with an extension are real 404s.
  app.use(async (c, next) => {
    if (hasDotSegment(c.req.path, (s) => s.startsWith(".")))
      return err(c, 404, "not_found", "Not found.");
    await next();
  });
  // Vite puts a content hash in every name under /assets/, so those never change.
  app.use(
    serveStatic({
      root: opts.staticDir,
      onFound: (_path, c) => {
        if (c.req.path.startsWith("/assets/"))
          c.header("Cache-Control", "public, max-age=31536000, immutable");
      },
    }),
  );
  const spa = serveStatic({ root: opts.staticDir, path: "index.html" });
  app.get("*", async (c, next) => {
    if (/\.[^/]+$/.test(c.req.path)) return err(c, 404, "not_found", "Not found.");
    return (await spa(c, next)) ?? undefined;
  });

  return app;
}
