/** @jsxImportSource preact */
import { type components, createApi } from "@ticketslinger/api-client";
import { type Context, Hono } from "hono";
import { bodyLimit } from "hono/body-limit";
import { serveStatic } from "hono/bun";
import type { VNode } from "preact";
import { render } from "preact-render-to-string";
import { csp } from "./csp";
import { EventPage, type EventPageProps, type StorefrontEvent } from "./pages/EventPage";
import { ErrorPage, NotFound } from "./pages/NotFound";

export type StorefrontOptions = {
  apiUrl: string;
  staticDir: string;
  /** Cloudflare Turnstile site key; "" turns the widget off. */
  turnstileSiteKey: string;
  /** Address of the TCP peer. Defaults to Bun's server.requestIP; tests inject their own. */
  peerIp?: (c: Context) => string | undefined;
  /** How we reach the API; tests inject their own. */
  fetch?: (req: Request) => Promise<Response>;
};

type ApiError = components["schemas"]["Error"]["error"];

// Forms only: a ticket form is a few hundred bytes.
const MAX_BODY = 64 * 1024;

// First URL segments the storefront keeps for itself. The API refuses them as organizer slugs.
const RESERVED = new Set(["checkout", "orders", "assets", "api", "healthz"]);

// Of the dashboard's request-header allowlist, the ones that describe the buyer. The rest
// (content-type, caching, idempotency-key) describe a request we now make ourselves.
const FORWARD = ["accept-language", "user-agent"];

// What the API takes from callers who aren't signed in (a UUID fits).
const IDEM = /^[A-Za-z0-9_-]{22,255}$/;

const GENERIC = "Something went wrong on our side. Try again.";
const NOT_READY = "This event can't take card payments just yet.";
const BUSY = "Busy for a moment. Try again.";
const SLOW_DOWN = "That's a lot of tries. Wait a minute and try again.";
// Our words for what createCheckout says. The API's own message never reaches the page.
const CHECKOUT_MESSAGES: Record<string, string> = {
  idempotency_in_progress: "We're already holding those for you. Give it a second.",
  payments_not_ready: NOT_READY,
  payments_not_configured: NOT_READY,
  try_again: BUSY,
  turnstile_unavailable: BUSY,
  rate_limited: SLOW_DOWN,
  promo_not_applicable: "That code doesn't apply to these tickets.",
  turnstile_failed: "One quick check that you're a person.",
};

type Status = 200 | 404 | 409 | 413 | 422 | 429 | 502 | 503;

const isLoopback = (ip: string) => /^127\./.test(ip) || ip === "::1" || /^::ffff:127\./i.test(ip);

function hasDotSegment(path: string): boolean {
  let decoded: string;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    return true;
  }
  return decoded.split(/[/\\]/).some((s) => s.startsWith("."));
}

export function createApp(opts: StorefrontOptions): Hono {
  const app = new Hono();
  const api = createApi(opts.apiUrl.replace(/\/+$/, ""), opts.fetch ?? ((r) => fetch(r)));
  const siteKey = opts.turnstileSiteKey;
  const peerIp =
    opts.peerIp ??
    ((c: Context) =>
      (c.env as { server?: Bun.Server<unknown> } | undefined)?.server?.requestIP(c.req.raw)
        ?.address);

  // Client IP is X-Real-IP only when the peer is our own reverse proxy (loopback).
  function clientIp(c: Context): string | undefined {
    const peer = peerIp(c);
    if (peer && isLoopback(peer)) return c.req.header("x-real-ip")?.trim() || peer;
    return peer;
  }

  // Every API call carries the buyer's address: the API's limits are per buyer, not per storefront.
  function apiHeaders(c: Context): Record<string, string> {
    const h: Record<string, string> = {};
    for (const k of FORWARD) {
      const v = c.req.header(k);
      if (v) h[k] = v;
    }
    const ip = clientIp(c);
    if (ip) h["x-real-ip"] = ip;
    return h;
  }

  function page(c: Context, status: Status, node: VNode, turnstile = false) {
    c.header("Content-Security-Policy", csp(turnstile ? "turnstile" : "plain"));
    // Each render carries its own idempotency key and live counts: never cache it.
    c.header("Cache-Control", "no-store");
    return c.html(`<!doctype html>${render(node)}`, status);
  }

  const notFound = (c: Context, what: "event" | "page" = "page") =>
    page(c, 404, <NotFound what={what} />);
  const trouble = (c: Context, status: Status = 502, message = GENERIC) =>
    page(c, status, <ErrorPage message={message} />);

  function eventPage(c: Context, status: Status, p: Omit<EventPageProps, "siteKey" | "idem">) {
    const form = siteKey !== "" && p.data.event.state === "on_sale";
    return page(c, status, <EventPage {...p} siteKey={siteKey} idem={crypto.randomUUID()} />, form);
  }

  // Too many code tries from this address still shows the event, just without the code.
  async function load(c: Context, organizer: string, eventSlug: string, code: string) {
    const r = await read(c, organizer, eventSlug, code);
    if (r.status !== 429 || !code) return { ...r, limited: false };
    return { ...(await read(c, organizer, eventSlug, "")), limited: true };
  }

  async function read(
    c: Context,
    organizer: string,
    eventSlug: string,
    code: string,
  ): Promise<{ status: number; data?: StorefrontEvent }> {
    try {
      const { data, response } = await api.GET("/storefront/{organizer}/{eventSlug}", {
        params: { path: { organizer, eventSlug }, query: code ? { code } : {} },
        headers: apiHeaders(c),
      });
      return { status: response.status, data };
    } catch {
      return { status: 502 };
    }
  }

  // Security headers on every response, including errors and static files.
  app.use(async (c, next) => {
    await next();
    const h = c.res.headers;
    h.set("X-Content-Type-Options", "nosniff");
    h.set("Referrer-Policy", "strict-origin-when-cross-origin");
    h.set("X-Frame-Options", "DENY");
    if (!h.has("Content-Security-Policy")) h.set("Content-Security-Policy", csp("plain"));
  });

  app.use(
    bodyLimit({
      maxSize: MAX_BODY,
      onError: (c) => trouble(c, 413, "That form is far bigger than ours. Go back and try again."),
    }),
  );

  // Dotfiles are never served.
  app.use("/assets/*", async (c, next) => {
    if (hasDotSegment(c.req.path)) return notFound(c);
    await next();
  });
  app.use(
    "/assets/*",
    serveStatic({ root: opts.staticDir, rewriteRequestPath: (p) => p.replace(/^\/assets/, "") }),
  );

  app.get("/:organizer/:event", async (c) => {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    const code = (c.req.query("code") ?? "").trim();
    const r = await load(c, organizer, event, code);
    if (r.status === 404) return notFound(c, "event");
    if (!r.data) return trouble(c);
    if (r.limited) {
      return eventPage(c, 429, { data: r.data, code: "", codeMessage: SLOW_DOWN });
    }
    const codeMessage = !code
      ? undefined
      : r.data.promo
        ? `Using code ${r.data.promo.code}.`
        : "That code doesn't work for this event.";
    return eventPage(c, 200, { data: r.data, code, codeMessage });
  });

  app.post("/:organizer/:event/checkout", async (c) => {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    let form: Record<string, unknown>;
    try {
      form = await c.req.parseBody();
    } catch {
      return trouble(c);
    }
    const field = (k: string) => {
      const v = form[k];
      return typeof v === "string" ? v : "";
    };

    // Only qty_<id> fields holding a whole number from 1 to 100; the rest is noise.
    const chosen: Record<string, number> = {};
    for (const [k, v] of Object.entries(form)) {
      if (!k.startsWith("qty_") || k.length === 4 || typeof v !== "string") continue;
      if (!/^\d{1,3}$/.test(v)) continue;
      const n = Number(v);
      if (n >= 1 && n <= 100) chosen[k.slice(4)] = n;
    }
    const items = Object.entries(chosen).map(([ticket_type_id, quantity]) => ({
      ticket_type_id,
      quantity,
    }));
    const code = field("code").trim();

    // Back to the event page with the buyer's picks and our words for what went wrong.
    const again = async (status: Status, message?: string, left?: Record<string, number>) => {
      const r = await load(c, organizer, event, code);
      if (r.status === 404) return notFound(c, "event");
      if (!r.data) return trouble(c, status);
      return eventPage(c, status, {
        data: r.data,
        code: r.limited ? "" : code,
        chosen,
        message,
        left,
      });
    };

    if (items.length === 0) return again(422, "Pick at least one ticket.");

    const idem = IDEM.test(field("idem")) ? field("idem") : crypto.randomUUID();
    const body: components["schemas"]["CreateCheckout"] = { organizer, event, items };
    if (code) body.promo_code = code;
    const turnstile = field("cf-turnstile-response");
    if (turnstile) body.turnstile_token = turnstile;

    const res = await api
      .POST("/checkouts", { body, headers: { ...apiHeaders(c), "Idempotency-Key": idem } })
      .catch(() => null);
    if (!res) return again(502, GENERIC);
    if (res.data?.token) {
      return c.redirect(`/checkout/${encodeURIComponent(res.data.token)}`, 303);
    }

    const status = res.response.status;
    const err: Partial<ApiError> = res.error?.error ?? {};
    const shown: Status = ([404, 409, 422, 429, 503] as const).find((s) => s === status) ?? 502;
    if (err.code === "sold_out") {
      const left: Record<string, number> = {};
      for (const a of err.available ?? []) {
        if ((chosen[a.ticket_type_id] ?? 0) > a.remaining) left[a.ticket_type_id] = a.remaining;
      }
      return again(409, Object.keys(left).length ? undefined : GENERIC, left);
    }
    return again(shown, CHECKOUT_MESSAGES[err.code ?? ""] ?? GENERIC);
  });

  app.notFound((c) => notFound(c));

  return app;
}
