/** @jsxImportSource preact */
import { createHash } from "node:crypto";
import { isIP } from "node:net";
import { type components, createApi } from "@ticketslinger/api-client";
import { type Context, Hono } from "hono";
import { bodyLimit } from "hono/body-limit";
import { serveStatic } from "hono/bun";
import type { VNode } from "preact";
import { render } from "preact-render-to-string";
import { EVENT_CANCELLED, LINK_BY_EMAIL } from "../islands/checkout";
import { ORDER, orderPath, TOKEN } from "../islands/links";
import { csp } from "./csp";
import { CheckoutPage } from "./pages/CheckoutPage";
import { EventPage, type EventPageProps, type StorefrontEvent } from "./pages/EventPage";
import { ErrorPage, NotFound, Unavailable } from "./pages/NotFound";
import { OrderLinkPage, type OrderLinkPageProps } from "./pages/OrderLinkPage";
import { NoOrder, OrderPage, type Sent } from "./pages/OrderPage";
import { Recent } from "./recent";

export type StorefrontOptions = {
  apiUrl: string;
  staticDir: string;
  /** Cloudflare Turnstile site key; "" turns the widget off. */
  turnstileSiteKey: string;
  /** Address of the TCP peer. Defaults to Bun's server.requestIP; tests inject their own. */
  peerIp?: (c: Context) => string | undefined;
  /** How we reach the API; tests inject their own. */
  fetch?: (req: Request) => Promise<Response>;
  /** How long a double tap waits for the first tap's checkout. Tests shorten it. */
  inProgressWaitMs?: number;
};

type ApiError = components["schemas"]["Error"]["error"];

// Forms: a ticket form is a few hundred bytes.
const MAX_BODY = 64 * 1024;
// The island's pay request: up to 100 tickets' answers, each up to 1,000 characters.
const MAX_JSON = 1024 * 1024;
// The most PDF the order page passes through from the API.
const MAX_PDF = 20 * 1024 * 1024;

// First URL segments the storefront keeps for itself. The API refuses them as organizer slugs.
const RESERVED = new Set(["checkout", "orders", "assets", "api", "healthz"]);

// Of the dashboard's request-header allowlist, the ones that describe the buyer. The rest
// (content-type, caching, idempotency-key) describe a request we now make ourselves.
const FORWARD = ["accept-language", "user-agent"];

// Paths that carry a checkout or order token.
const TOKEN_PATH = /^\/(checkout|orders|api\/checkouts)\//;

// What the API takes from callers who aren't signed in (a UUID fits).
const IDEM = /^[A-Za-z0-9_-]{22,255}$/;
// What the picker can send: 0 to 100, no padding.
const QTY = /^(0|[1-9][0-9]?|100)$/;
// More ticket types than any event page shows.
const MAX_TYPES = 50;

const GENERIC = "Something went wrong on our side. Try again.";
const NOT_READY = "This event can't take card payments just yet.";
const BUSY = "Busy for a moment. Try again.";
const SLOW_DOWN = "That's a lot of tries. Wait a minute and try again.";
// The order page's notices after "Email me my tickets again", and the API's 409s that lead to
// them. no_tickets says nothing: the page it goes back to no longer offers the button.
const SENT: readonly Sent[] = ["1", "soon", "pending"];
const SENT_FOR: Record<string, Sent | ""> = {
  email_on_its_way: "pending",
  too_soon: "soon",
  no_tickets: "",
};
// Our words for what createCheckout says. The API's own message never reaches the page.
const CHECKOUT_MESSAGES: Record<string, string> = {
  idempotency_in_progress: "We're already holding those for you. Give it a second.",
  payments_not_ready: NOT_READY,
  payments_not_configured: NOT_READY,
  try_again: BUSY,
  turnstile_unavailable: BUSY,
  rate_limited: SLOW_DOWN,
  turnstile_failed: "Finish the quick check above Get tickets, then try again.",
  // Races a buyer can lose while the page sits open: the list they get back is fresh.
  sales_closed:
    "Sales closed on one of those tickets while you were choosing. The list below is up to date, so pick again from what's still on sale.",
  sales_not_open:
    "One of those tickets isn't on sale yet. The list below shows when it opens, so pick from what's on sale now or come back then.",
  unknown_ticket_type:
    "One of those tickets isn't on sale any more. We've refreshed the list, so pick again from what's here.",
  quantity_out_of_range:
    "The per-order limit on one of those tickets changed. Pick again from the list below, which shows the new limits.",
  too_many_tickets:
    "One order holds up to 100 tickets. Pick fewer, or split them across two orders.",
  no_items: "Pick at least one ticket.",
  event_over:
    "This event finished while you were choosing, so its tickets are off sale. You haven't been charged, and there's nothing more to do.",
};
// The promo refusals go next to the code box, where the buyer can fix them.
const CODE_MESSAGES: Record<string, string> = {
  promo_used_up:
    "Every use of that code has gone. Clear it to buy at the regular price, or try another.",
  promo_invalid:
    "That code isn't working for this event. Check the spelling, or clear it to buy at the regular price.",
  promo_not_applicable:
    "That code doesn't take anything off these tickets. Pick the ones it's for, or clear it.",
};
const IN_PROGRESS = CHECKOUT_MESSAGES.idempotency_in_progress;
// Our words for what requestOrderLinks says.
const BAD_EMAIL = "That email doesn't look right. It should look like ada@example.com.";
const LINK_MESSAGES: Record<string, string> = {
  rate_limited: "That's a lot of tries from here. Wait a while and try again.",
  turnstile_failed: "We couldn't check you're a person. Try again.",
  turnstile_unavailable: BUSY,
};

type Status = 200 | 400 | 404 | 409 | 410 | 413 | 422 | 429 | 500 | 502 | 503;

// How a checkout request ended: its checkout, or the page we showed for it.
type Outcome =
  | { kind: "made"; token: string }
  | {
      kind: "failed";
      status: Status;
      message?: string;
      codeMessage?: string;
      left?: Record<string, number>;
    };

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

function outcomeOf(
  status: number,
  token: string | undefined,
  err: Partial<ApiError>,
  chosen: Record<string, number>,
): Outcome {
  if (status === 201) {
    return token && TOKEN.test(token)
      ? { kind: "made", token }
      : { kind: "failed", status: 502, message: GENERIC };
  }
  if (err.code === "sold_out") {
    const left: Record<string, number> = {};
    for (const a of err.available ?? []) {
      if ((chosen[a.ticket_type_id] ?? 0) > a.remaining) left[a.ticket_type_id] = a.remaining;
    }
    const someLeft = Object.values(left).some((n) => n > 0);
    return {
      kind: "failed",
      status: 409,
      message: someLeft ? undefined : "Those tickets just sold out.",
      left,
    };
  }
  const shown = ([404, 409, 422, 429, 503] as const).find((s) => s === status) ?? 502;
  const codeMessage = CODE_MESSAGES[err.code ?? ""];
  if (codeMessage) return { kind: "failed", status: shown, codeMessage };
  return { kind: "failed", status: shown, message: CHECKOUT_MESSAGES[err.code ?? ""] ?? GENERIC };
}

export function createApp(opts: StorefrontOptions): Hono {
  const app = new Hono();
  const api = createApi(opts.apiUrl.replace(/\/+$/, ""), opts.fetch ?? ((r) => fetch(r)));
  const siteKey = opts.turnstileSiteKey;
  const recent = new Recent<Outcome>();
  const waitMs = opts.inProgressWaitMs ?? 3000;
  const peerIp =
    opts.peerIp ??
    ((c: Context) =>
      (c.env as { server?: Bun.Server<unknown> } | undefined)?.server?.requestIP(c.req.raw)
        ?.address);

  // Client IP is X-Real-IP only when the peer is our own reverse proxy (loopback).
  function clientIp(c: Context): string | undefined {
    const peer = peerIp(c);
    if (peer && isLoopback(peer)) {
      const real = c.req.header("x-real-ip")?.trim() ?? "";
      return isIP(real) ? real : peer;
    }
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

  function page(
    c: Context,
    status: Status,
    node: VNode,
    policy: Parameters<typeof csp>[0] = "plain",
  ) {
    c.header("Content-Security-Policy", csp(policy));
    // Each render carries its own idempotency key and live counts: never cache it.
    c.header("Cache-Control", "no-store");
    return c.html(`<!doctype html>${render(node)}`, status);
  }

  const notFound = (c: Context, what: "event" | "page" | "checkout" = "page") =>
    page(c, 404, <NotFound what={what} />);
  // 410 from the storefront read, createCheckout, getCheckout or pay: the organizer is suspended.
  const unavailable = (c: Context, charged = false) =>
    page(c, 410, <Unavailable charged={charged} />);
  const trouble = (c: Context, status: Status = 502, message = GENERIC) =>
    page(c, status, <ErrorPage message={message} />);

  function eventPage(
    c: Context,
    status: Status,
    p: Omit<EventPageProps, "siteKey" | "idem"> & { idem?: string },
  ) {
    const form = siteKey !== "" && p.data.event.state === "on_sale";
    const idem = p.idem ?? crypto.randomUUID();
    return page(
      c,
      status,
      <EventPage {...p} siteKey={siteKey} idem={idem} />,
      form ? "turnstile" : "plain",
    );
  }

  // The first tap of a double tap is still running: give it a moment to finish.
  async function firstTap(key: string): Promise<Outcome | undefined> {
    for (let waited = 0; waited < waitMs; waited += 50) {
      const outcome = recent.get(key);
      if (outcome) return outcome;
      await Bun.sleep(50);
    }
    return recent.get(key);
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
    // A page that set its own policy keeps it. Token paths never send a Referer at all,
    // not even to our own stylesheet: the token would land in the access log.
    if (!h.has("Referrer-Policy")) {
      h.set(
        "Referrer-Policy",
        TOKEN_PATH.test(c.req.path) ? "no-referrer" : "strict-origin-when-cross-origin",
      );
    }
    h.set("X-Frame-Options", "DENY");
    if (!h.has("Content-Security-Policy")) h.set("Content-Security-Policy", csp("plain"));
  });

  const tooBig = bodyLimit({
    maxSize: MAX_BODY,
    onError: (c) => trouble(c, 413, "That form is far bigger than ours. Go back and try again."),
  });
  app.use(async (c, next) => (c.req.path.startsWith("/api/") ? next() : tooBig(c, next)));
  app.use(
    "/api/*",
    bodyLimit({ maxSize: MAX_JSON, onError: (c) => apiError(c, 413, "too_large") }),
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

  app.get("/healthz", (c) => c.text("ok"));
  // No icon yet; an empty answer keeps a 404 out of every buyer's console.
  app.get("/favicon.ico", (c) => c.body(null, 204));

  async function getCheckout(c: Context, token: string) {
    try {
      const { data, error, response } = await api.GET("/checkouts/{token}", {
        params: { path: { token } },
        headers: apiHeaders(c),
      });
      return { status: response.status, data, error };
    } catch {
      return { status: 502 };
    }
  }

  app.get("/checkout/:token", async (c) => {
    const token = c.req.param("token");
    if (!TOKEN.test(token)) return notFound(c, "checkout");
    const r = await getCheckout(c, token);
    if (r.status === 404) return notFound(c, "checkout");
    if (r.status === 410) {
      // Back from 3-D Secure (Stripe's return_url is this page): a card may have been charged.
      const q = c.req.query();
      return unavailable(c, "payment_intent" in q || "redirect_status" in q);
    }
    if (r.status === 429) return trouble(c, 429, SLOW_DOWN);
    if (r.status === 409 && r.error?.error.code === "event_cancelled") {
      return trouble(c, 409, EVENT_CANCELLED);
    }
    const checkout = r.data;
    if (!checkout?.event) return trouble(c);
    if (checkout.status === "completed") {
      // The buyer asked for a new link since: this address no longer hands it out.
      if (!checkout.order_url) {
        return page(c, 200, <ErrorPage title="Your tickets" message={LINK_BY_EMAIL} />);
      }
      // Rebuilt from the code and token: the link's own path never reaches the Location header.
      const to = orderPath(checkout.order_url ?? "", checkout.order_code);
      if (!to) return trouble(c);
      c.header("Cache-Control", "no-store");
      return c.redirect(to, 303);
    }
    const node = <CheckoutPage token={token} checkout={checkout} event={checkout.event} />;
    // Over: the hold ran out, or its payment was stopped. Nothing left to pay here.
    return checkout.status === "expired" || checkout.status === "cancelled"
      ? page(c, 410, node)
      : page(c, 200, node, "stripe");
  });

  // The island's two calls, same-origin, passed through with the buyer's address.
  const apiError = (c: Context, status: number, code: string) => {
    c.header("Cache-Control", "no-store");
    return c.json({ error: { code, message: code } }, status as 400);
  };
  const relay = (c: Context, status: number, body: unknown, retryAfter?: string | null) => {
    c.header("Cache-Control", "no-store");
    if (retryAfter) c.header("Retry-After", retryAfter);
    return c.json(body ?? {}, status as 200);
  };

  app.get("/api/checkouts/:token", async (c) => {
    const token = c.req.param("token");
    if (!TOKEN.test(token)) return apiError(c, 404, "not_found");
    const r = await getCheckout(c, token);
    if (r.status === 502) return apiError(c, 502, "unreachable");
    return relay(c, r.status, r.data ?? r.error);
  });

  app.post("/api/checkouts/:token/pay", async (c) => {
    const token = c.req.param("token");
    if (!TOKEN.test(token)) return apiError(c, 404, "not_found");
    const key = c.req.header("idempotency-key") ?? "";
    if (!IDEM.test(key)) return apiError(c, 400, "bad_idempotency_key");
    // JSON only: a cross-site form can't send it without asking first.
    if (!/^application\/json\b/i.test(c.req.header("content-type") ?? "")) {
      return apiError(c, 415, "json_only");
    }
    let body: components["schemas"]["PayCheckout"];
    try {
      body = await c.req.json();
    } catch {
      return apiError(c, 400, "bad_json");
    }
    try {
      const { data, error, response } = await api.POST("/checkouts/{token}/pay", {
        params: { path: { token } },
        body,
        headers: { ...apiHeaders(c), "Idempotency-Key": key },
      });
      return relay(c, response.status, data ?? error, response.headers.get("retry-after"));
    } catch {
      return apiError(c, 502, "unreachable");
    }
  });

  app.get("/orders/:code", async (c) => {
    const code = c.req.param("code");
    const t = c.req.query("t") ?? "";
    if (!ORDER.test(code) || !TOKEN.test(t)) return page(c, 404, <NoOrder />);
    let status = 502;
    let order: components["schemas"]["Order"] | undefined;
    try {
      const r = await api.GET("/orders/{code}", {
        params: { path: { code }, query: { token: t } },
        headers: apiHeaders(c),
      });
      status = r.response.status;
      order = r.data;
    } catch {}
    const sent = SENT.find((s) => s === c.req.query("sent"));
    if (order) return page(c, 200, <OrderPage order={order} token={t} sent={sent} />, "order");
    if (status === 404 || status === 400) return page(c, 404, <NoOrder />);
    if (status === 429) return trouble(c, 429, SLOW_DOWN);
    return trouble(c);
  });

  // The API's PDF, streamed through with its own headers; anything else is the order 404.
  app.get("/orders/:code/tickets.pdf", async (c) => {
    const code = c.req.param("code");
    const t = c.req.query("t") ?? "";
    if (!ORDER.test(code) || !TOKEN.test(t)) return page(c, 404, <NoOrder />);
    const r = await api
      .GET("/orders/{code}/tickets.pdf", {
        params: { path: { code }, query: { token: t } },
        headers: apiHeaders(c),
        parseAs: "stream",
      })
      .catch(() => null);
    if (!r) return trouble(c);
    const status = r.response.status;
    if (status === 200 && r.data) {
      // A PDF of 100 tickets is well under a megabyte: no length, or a huge one, isn't ours.
      const length = Number(r.response.headers.get("content-length") ?? NaN);
      if (!(length >= 0 && length <= MAX_PDF)) {
        await r.data.cancel().catch(() => {});
        return trouble(c);
      }
      const h = new Headers({ "cache-control": "no-store" });
      for (const k of ["content-type", "content-disposition", "content-length"]) {
        const v = r.response.headers.get(k);
        if (v) h.set(k, v);
      }
      return new Response(r.data, { status: 200, headers: h });
    }
    // Nothing valid left: the order page says why, and no longer offers the PDF.
    if (status === 409 && r.error?.error.code === "no_tickets") {
      return c.redirect(`/orders/${code}?t=${encodeURIComponent(t)}`, 303);
    }
    if (status === 429) return trouble(c, 429, SLOW_DOWN);
    if (status >= 500) return trouble(c);
    return page(c, 404, <NoOrder />);
  });

  // "Email me my tickets again": back to the order page, which says how it went.
  app.post("/orders/:code/email", async (c) => {
    const code = c.req.param("code");
    let t = "";
    try {
      const v = (await c.req.parseBody()).t;
      t = typeof v === "string" ? v : "";
    } catch {}
    if (!ORDER.test(code) || !TOKEN.test(t)) return page(c, 404, <NoOrder />);
    const r = await api
      .POST("/orders/{code}/email", {
        params: { path: { code } },
        body: { token: t },
        headers: apiHeaders(c),
      })
      .catch(() => null);
    if (!r) return trouble(c);
    const status = r.response.status;
    const back = `/orders/${code}?t=${encodeURIComponent(t)}`;
    if (status === 202) return c.redirect(`${back}&sent=1`, 303);
    const reason = r.error?.error.code ?? "";
    const answer = Object.hasOwn(SENT_FOR, reason) ? SENT_FOR[reason] : undefined;
    if (status === 409 && answer !== undefined) {
      return c.redirect(answer ? `${back}&sent=${answer}` : back, 303);
    }
    if (status === 429) return trouble(c, 429, SLOW_DOWN);
    if (status >= 500) return trouble(c);
    return page(c, 404, <NoOrder />);
  });

  app.get("/:organizer/:event", async (c) => {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    const code = (c.req.query("code") ?? "").trim();
    const r = await load(c, organizer, event, code);
    if (r.status === 404) return notFound(c, "event");
    if (r.status === 410) return unavailable(c);
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

  // "Email me my link again": the same answer whether or not the address bought anything.
  async function linkPage(
    c: Context,
    status: Status,
    p: Omit<OrderLinkPageProps, "eventName" | "action" | "siteKey">,
  ) {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    const r = await read(c, organizer, event, "");
    if (r.status === 404) return notFound(c, "event");
    // Off sale, but buyers who already paid still get their links: just no event name.
    if (!r.data && r.status !== 410) return trouble(c);
    const action = `/${encodeURIComponent(organizer)}/${encodeURIComponent(event)}/order-link`;
    const form = siteKey !== "" && !p.done;
    return page(
      c,
      status,
      <OrderLinkPage {...p} eventName={r.data?.event.name} action={action} siteKey={siteKey} />,
      form ? "turnstile" : "plain",
    );
  }

  app.get("/:organizer/:event/order-link", (c) => linkPage(c, 200, {}));

  app.post("/:organizer/:event/order-link", async (c) => {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    let form: Record<string, unknown>;
    try {
      form = await c.req.parseBody();
    } catch {
      return trouble(c, 400, "We couldn't read that form. Go back and try again.");
    }
    const field = (k: string) => {
      const v = form[k];
      return typeof v === "string" ? v : "";
    };
    const email = field("email").trim().slice(0, 254);
    const body: components["schemas"]["OrderLinksRequest"] = { email };
    const turnstile = field("cf-turnstile-response");
    if (turnstile) body.turnstile_token = turnstile;
    const res = await api
      .POST("/storefront/{organizer}/{eventSlug}/order-links", {
        params: { path: { organizer, eventSlug: event } },
        body,
        headers: { ...apiHeaders(c), "Idempotency-Key": crypto.randomUUID() },
      })
      .catch(() => null);
    if (!res) return linkPage(c, 502, { email, message: GENERIC });
    const status = res.response.status;
    if (status === 204) return linkPage(c, 200, { done: true });
    if (status === 404) return notFound(c, "event");
    const code = res.error?.error.code ?? "";
    if (code === "bad_email") return linkPage(c, 422, { email, emailMessage: BAD_EMAIL });
    const shown = ([400, 422, 429, 503] as const).find((s) => s === status) ?? 502;
    return linkPage(c, shown, { email, message: LINK_MESSAGES[code] ?? GENERIC });
  });

  app.post("/:organizer/:event/checkout", async (c) => {
    const { organizer, event } = c.req.param();
    if (RESERVED.has(organizer)) return notFound(c);
    let form: Record<string, unknown>;
    try {
      form = await c.req.parseBody();
    } catch {
      return trouble(c, 400, "We couldn't read that form. Go back and try again.");
    }
    const field = (k: string) => {
      const v = form[k];
      return typeof v === "string" ? v : "";
    };

    const idem = IDEM.test(field("idem")) ? field("idem") : crypto.randomUUID();

    // qty_<id> fields hold what the picker offers, 0 to 100. Anything else wasn't our form.
    const chosen: Record<string, number> = {};
    const qty = Object.entries(form).filter(([k]) => k.startsWith("qty_"));
    const tampered =
      qty.length > MAX_TYPES ||
      qty.some(([k, v]) => k.length === 4 || typeof v !== "string" || !QTY.test(v));
    if (!tampered) {
      for (const [k, v] of qty) if (v !== "0") chosen[k.slice(4)] = Number(v);
    }
    const items = Object.entries(chosen).map(([ticket_type_id, quantity]) => ({
      ticket_type_id,
      quantity,
    }));
    const code = field("code").trim();

    // Back to the event page with the buyer's picks and our words for what went wrong.
    const again = async (
      status: Status,
      message?: string,
      left?: Record<string, number>,
      keep?: string,
      codeMessage?: string,
    ) => {
      const r = await load(c, organizer, event, code);
      if (r.status === 404) return notFound(c, "event");
      if (r.status === 410) return unavailable(c);
      if (!r.data) return trouble(c, status);
      return eventPage(c, status, {
        data: r.data,
        code: r.limited ? "" : code,
        chosen,
        message,
        codeMessage,
        left,
        idem: keep,
      });
    };

    if (tampered) return again(422, "Pick your tickets from the lists, then try again.");
    if (items.length === 0) return again(422, "Pick at least one ticket.");

    // The whole request, not just its key: a changed form, another event or another buyer
    // with the same idem is a new attempt.
    items.sort((x, y) => (x.ticket_type_id < y.ticket_type_id ? -1 : 1));
    const key = createHash("sha256")
      .update(JSON.stringify([idem, clientIp(c) ?? "", organizer, event, items, code]))
      .digest("base64url");
    const show = (o: Outcome) =>
      o.kind === "made"
        ? c.redirect(`/checkout/${o.token}`, 303)
        : again(o.status, o.message, o.left, undefined, o.codeMessage);
    // This exact request already made a checkout: go back to it rather than holding a second.
    const before = recent.get(key);
    if (before?.kind === "made") return show(before);
    // A double tap waits for this attempt, not an earlier one that failed.
    recent.delete(key);

    const body: components["schemas"]["CreateCheckout"] = { organizer, event, items };
    if (code) body.promo_code = code;
    const turnstile = field("cf-turnstile-response");
    if (turnstile) body.turnstile_token = turnstile;

    const res = await api
      .POST("/checkouts", { body, headers: { ...apiHeaders(c), "Idempotency-Key": idem } })
      .catch(() => null);
    if (!res) return again(502, GENERIC);
    if (res.response.status === 410) return unavailable(c);
    const err: Partial<ApiError> = res.error?.error ?? {};
    if (err.code === "idempotency_in_progress") {
      const first = await firstTap(key);
      if (first) return show(first);
      // Same key in the new form, so the next tap also finds the first tap's checkout.
      return again(409, IN_PROGRESS, undefined, idem);
    }
    const outcome = outcomeOf(res.response.status, res.data?.token, err, chosen);
    recent.set(key, outcome);
    return show(outcome);
  });

  app.notFound((c) => notFound(c));
  // An error can carry a URL we fetched, token and all: log its kind, the route pattern and its
  // message with every URL and path cut out. Never the raw error or its stack.
  app.onError((e, c) => {
    const message = e.message.replace(/[a-z][a-z0-9+.-]*:\/\/\S*|\/\S*/gi, "<url>").slice(0, 200);
    console.error(`${e.name}: ${message} (${c.req.method} ${c.req.routePath})`);
    return trouble(c, 500);
  });

  return app;
}
