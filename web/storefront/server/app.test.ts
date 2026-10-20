import { expect, spyOn, test } from "bun:test";
import { createApp } from "./app";
import { priceNote } from "./pages/EventPage";

const event = {
  organizer: { name: "Acme", slug: "acme" },
  event: {
    id: "e1",
    slug: "gala",
    name: "Spring Gala",
    description: "",
    venue_name: "The Hall",
    venue_address: "",
    timezone: "America/Denver",
    starts_at: "2027-05-02T01:00:00Z",
    ends_at: "2027-05-02T05:00:00Z",
    fee_mode: "pass",
    state: "on_sale",
  },
  ticket_types: [
    {
      id: "t1",
      name: "General",
      description: "",
      price_cents: 2500,
      all_in_cents: 2600,
      all_in_fee_cents: 100,
      all_in_tax_cents: 0,
      min_per_order: 1,
      max_per_order: 10,
      remaining: 3,
      on_sale: true,
      sales_start: null,
      sales_end: null,
      unlocked: false,
    },
  ],
  promo: null as null | { code: string; kind: string; value: number },
  questions: [],
};

type Api = (req: Request) => Response | Promise<Response>;

function app(api: Api, opts: { peer?: string; siteKey?: string } = {}) {
  const calls: Request[] = [];
  const a = createApp({
    apiUrl: "http://api/v1",
    staticDir: "./dist",
    turnstileSiteKey: opts.siteKey ?? "1x00000000000000000000AA",
    inProgressWaitMs: 300,
    peerIp: () => opts.peer ?? "127.0.0.1",
    fetch: async (r) => {
      calls.push(r.clone());
      return api(r);
    },
  });
  return { a, calls };
}

const KEY = "0f8fad5b-d9cb-469f-a165-70867728950e";
// What the API hands out: 32 random bytes, base64url.
const TOK = "q3Jx0_Vb-8sKd2LmN4pR7tWzYcE1gH5iA9oU6fXnBvM";
const created = () => Response.json({ token: TOK, status: "open" }, { status: 201 });
const checkoutCalls = (calls: Request[]) =>
  calls.filter((c) => new URL(c.url).pathname === "/v1/checkouts").length;
const apiError = (status: number, code: string, extra: Record<string, unknown> = {}) =>
  Response.json({ error: { code, message: "<b>API words</b>", ...extra } }, { status });

// Answers the storefront read with the event, and createCheckout with `checkout`.
const routes =
  (checkout: () => Response | Promise<Response>, ev: unknown = event): Api =>
  (r) =>
    new URL(r.url).pathname.startsWith("/v1/storefront/") ? Response.json(ev) : checkout();

const form = (fields: Record<string, string>, ip = "203.0.113.9") => ({
  method: "POST",
  body: new URLSearchParams(fields),
  headers: { "x-real-ip": ip },
});

test("event page is server-rendered; its one script of ours is the Turnstile loader", async () => {
  const { a } = app(() => Response.json(event));
  const res = await a.request("/acme/gala");
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).toContain("Spring Gala");
  expect(html).toContain("$26.00");
  expect(html.match(/<script[^>]*>/gi)).toEqual([
    '<script type="module" src="/assets/turnstile.js">',
  ]);
  expect(html).not.toMatch(/style="/);
  // The quantity picker can't offer more than are left.
  expect(html).toContain('<option value="3">3</option>');
  expect(html).not.toContain('<option value="4">');
  expect(res.headers.get("content-security-policy")).toContain(
    "script-src 'self' https://challenges.cloudflare.com",
  );
  expect(html).toContain('data-sitekey="1x00000000000000000000AA"');
  expect(html).not.toContain("A service fee is added at checkout.");
  expect(html).toContain("Includes $1.00 service fee");
  expect(html).toContain('action="/acme/gala/checkout"');
  expect(html).toMatch(/name="idem" value="[0-9a-f-]{36}"/);
});

test("without a site key there's no widget and no Cloudflare in the policy", async () => {
  const { a } = app(() => Response.json(event), { siteKey: "" });
  const res = await a.request("/acme/gala");
  const html = await res.text();
  expect(html).not.toContain("cf-turnstile");
  expect(html).not.toContain("<script");
  expect(res.headers.get("content-security-policy")).not.toContain("cloudflare");
});

test("descriptions and names are text, never HTML", async () => {
  const ev = structuredClone(event);
  ev.event.description = "Bring a coat.\n\n<script>alert(1)</script>";
  ev.ticket_types[0].name = "<img src=x onerror=alert(1)>";
  const { a } = app(() => Response.json(ev), { siteKey: "" });
  const html = await (await a.request("/acme/gala")).text();
  expect(html).not.toContain("<script>alert(1)</script>");
  expect(html).toContain("&lt;script>alert(1)&lt;/script>");
  expect(html).not.toContain("<img src=x");
  expect(html).toContain('<p class="description">Bring a coat.</p>');
});

test("tickets not on sale yet, sold out, and closed", async () => {
  const ev = structuredClone(event);
  ev.ticket_types = [
    { ...ev.ticket_types[0], id: "t1", remaining: 0 },
    {
      ...ev.ticket_types[0],
      id: "t2",
      name: "Early",
      on_sale: false,
      sales_start: "2027-05-01T16:00:00Z" as unknown as null,
    },
    { ...ev.ticket_types[0], id: "t3", name: "Late", on_sale: false },
  ];
  const { a } = app(() => Response.json(ev));
  const html = await (await a.request("/acme/gala")).text();
  expect(html).toContain("Sold out");
  expect(html).toContain("On sale May 1, 10:00 AM");
  expect(html).not.toContain('name="qty_t1"');
  expect(html).not.toContain('name="qty_t2"');
  expect(html).not.toContain('name="qty_t3"');
});

test("an ended event has no form", async () => {
  const ev = structuredClone(event);
  ev.event.state = "ended";
  ev.ticket_types[0].on_sale = false;
  const { a } = app(() => Response.json(ev));
  const res = await a.request("/acme/gala");
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).toContain("This event has ended.");
  expect(html).not.toContain("<form");
  expect(res.headers.get("content-security-policy")).not.toContain("cloudflare");
});

test("unknown event is the 404 page", async () => {
  const { a } = app(() => apiError(404, "not_found"));
  const res = await a.request("/acme/nope");
  expect(res.status).toBe(404);
  const html = await res.text();
  expect(html).toContain("We can't find that event.");
  expect(html).not.toContain("API words");
});

test("the storefront's own paths never reach the API", async () => {
  const { a, calls } = app(() => Response.json(event));
  for (const p of [
    "/checkout/gala",
    "/orders/x",
    "/assets/x",
    "/api/x",
    "/healthz/x",
    "/",
    "/acme",
  ]) {
    expect((await a.request(p)).status).toBe(404);
  }
  expect(calls).toHaveLength(0);
});

test("a code goes to the API with the buyer's address and stays in the form", async () => {
  const ev = structuredClone(event);
  ev.promo = { code: "FRIENDS", kind: "percent", value: 2000 };
  const { a, calls } = app(() => Response.json(ev));
  const res = await a.request("/acme/gala?code=FRIENDS", {
    headers: { "x-real-ip": "203.0.113.9" },
  });
  const html = await res.text();
  expect(new URL(calls[0].url).searchParams.get("code")).toBe("FRIENDS");
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(html).toContain('<input type="hidden" name="code" value="FRIENDS"/>');
});

test("a code the API won't take isn't kept", async () => {
  const { a } = app(() => Response.json(event));
  const html = await (await a.request("/acme/gala?code=NOPE")).text();
  expect(html).not.toContain('name="code" value="NOPE"/><');
  expect(html).not.toContain('type="hidden" name="code" value="NOPE"');
});

test("too many code tries still shows the event, without the code", async () => {
  const seen: (string | null)[] = [];
  const { a } = app((r) => {
    const code = new URL(r.url).searchParams.get("code");
    seen.push(code);
    return code ? apiError(429, "rate_limited") : Response.json(event);
  });
  const res = await a.request("/acme/gala?code=X");
  const html = await res.text();
  expect(res.status).toBe(429);
  expect(seen).toEqual(["X", null]);
  expect(html).toContain("That's a lot of tries. Wait a minute and try again.");
  expect(html).toContain("Spring Gala");
});

test("buying posts to the API with an idempotency key and the buyer's address, then redirects", async () => {
  const { a, calls } = app(() => created());
  const body = new URLSearchParams({
    qty_t1: "2",
    idem: KEY,
    "cf-turnstile-response": "XXXX.DUMMY.TOKEN.XXXX",
  });
  const res = await a.request("/acme/gala/checkout", {
    method: "POST",
    body,
    headers: { "x-real-ip": "203.0.113.9" },
  });
  expect(res.status).toBe(303);
  expect(res.headers.get("location")).toBe(`/checkout/${TOK}`);
  const sent = calls[0];
  expect(new URL(sent.url).pathname).toBe("/v1/checkouts");
  expect(sent.headers.get("idempotency-key")).toBe(KEY);
  expect(sent.headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(await sent.json()).toEqual({
    organizer: "acme",
    event: "gala",
    items: [{ ticket_type_id: "t1", quantity: 2 }],
    turnstile_token: "XXXX.DUMMY.TOKEN.XXXX",
  });
});

test("a missing or malformed idem becomes a fresh UUID", async () => {
  for (const idem of [undefined, "k-1", "x".repeat(21), `${"a".repeat(30)}!`]) {
    const { a, calls } = app(() => created());
    const f: Record<string, string> = { qty_t1: "1" };
    if (idem !== undefined) f.idem = idem;
    expect((await a.request("/acme/gala/checkout", form(f))).status).toBe(303);
    expect(calls[0].headers.get("idempotency-key")).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );
  }
});

test("whole quantities 0-100 are sent with the code; other fields are ignored", async () => {
  const { a, calls } = app(created);
  await a.request(
    "/acme/gala/checkout",
    form({ qty_t1: "2", qty_t2: "0", qty_t3: "100", other: "9", code: "FRIENDS", idem: KEY }),
  );
  expect(await calls[0].json()).toEqual({
    organizer: "acme",
    event: "gala",
    items: [
      { ticket_type_id: "t1", quantity: 2 },
      { ticket_type_id: "t3", quantity: 100 },
    ],
    promo_code: "FRIENDS",
  });
});

test("a tampered quantity is refused without trying to buy", async () => {
  for (const bad of ["1.5", "101", "-1", "abc", "01", "", " 2"]) {
    const { a, calls } = app(routes(created));
    const res = await a.request(
      "/acme/gala/checkout",
      form({ qty_t1: "2", qty_t2: bad, idem: KEY }),
    );
    expect(res.status).toBe(422);
    expect(await res.text()).toContain("Pick your tickets from the lists, then try again.");
    expect(checkoutCalls(calls)).toBe(0);
  }
});

test("more than 50 ticket types in one form is refused without trying to buy", async () => {
  const f: Record<string, string> = { idem: KEY };
  for (let i = 0; i < 51; i++) f[`qty_t${i}`] = "1";
  const { a, calls } = app(routes(created));
  const res = await a.request("/acme/gala/checkout", form(f));
  expect(res.status).toBe(422);
  expect(checkoutCalls(calls)).toBe(0);
});

test("a token that doesn't look like ours is never a redirect", async () => {
  const { a } = app(routes(() => Response.json({ token: "../x" }, { status: 201 })));
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "1", idem: KEY }));
  expect(res.status).toBe(502);
  expect(res.headers.get("location")).toBeNull();
  expect(await res.text()).toContain("Something went wrong on our side. Try again.");
});

test("a double tap waits for the first tap and lands on the same checkout", async () => {
  let running = false;
  let made = 0;
  const { a } = app(
    routes(async () => {
      if (running) return apiError(409, "idempotency_in_progress");
      running = true;
      await Bun.sleep(100);
      made++;
      return created();
    }),
  );
  const tap = () => a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  const first = tap();
  await Bun.sleep(10);
  const [r1, r2] = await Promise.all([first, tap()]);
  expect(made).toBe(1);
  for (const r of [r1, r2]) {
    expect(r.status).toBe(303);
    expect(r.headers.get("location")).toBe(`/checkout/${TOK}`);
  }
});

test("a later tap of the same form goes to the same checkout without asking again", async () => {
  const { a, calls } = app(routes(created));
  const tap = () => a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect((await tap()).headers.get("location")).toBe(`/checkout/${TOK}`);
  const again = await tap();
  expect(again.status).toBe(303);
  expect(again.headers.get("location")).toBe(`/checkout/${TOK}`);
  expect(checkoutCalls(calls)).toBe(1);
});

test("the same key with a different request is a new attempt", async () => {
  const other = "Zz9_Yy8-Xx7_Ww6-Vv5_Uu4-Tt3_Ss2-Rr1_Qq0-Pp9";
  let n = 0;
  const { a, calls } = app(
    routes(() => Response.json({ token: n++ ? other : TOK, status: "open" }, { status: 201 })),
  );
  const first = await a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect(first.headers.get("location")).toBe(`/checkout/${TOK}`);
  const variants: [string, Record<string, string>, string?][] = [
    ["/acme/gala/checkout", { qty_t1: "3", idem: KEY }],
    ["/acme/gala/checkout", { qty_t1: "2", code: "FRIENDS", idem: KEY }],
    ["/acme/other/checkout", { qty_t1: "2", idem: KEY }],
    ["/acme/gala/checkout", { qty_t1: "2", idem: KEY }, "198.51.100.20"],
  ];
  for (const [path, fields, ip] of variants) {
    const before = checkoutCalls(calls);
    const res = await a.request(path, form(fields, ip));
    expect(checkoutCalls(calls)).toBe(before + 1);
    expect(res.headers.get("location")).toBe(`/checkout/${other}`);
  }
});

test("a double tap whose first tap failed shows that failure", async () => {
  let running = false;
  const { a } = app(
    routes(async () => {
      if (running) return apiError(409, "idempotency_in_progress");
      running = true;
      await Bun.sleep(100);
      return apiError(409, "sold_out", { available: [{ ticket_type_id: "t1", remaining: 0 }] });
    }),
  );
  const tap = () => a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  const first = tap();
  await Bun.sleep(10);
  const [r1, r2] = await Promise.all([first, tap()]);
  for (const r of [r1, r2]) {
    expect(r.status).toBe(409);
    const html = await r.text();
    expect(html).toContain("Those tickets just sold out.");
    expect(html).not.toContain("already holding");
  }
});

test("still in progress after the wait: the page keeps the same key", async () => {
  const { a } = app(routes(() => apiError(409, "idempotency_in_progress")));
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect(res.status).toBe(409);
  const html = await res.text();
  expect(html).toContain("We're already holding those for you. Give it a second.");
  expect(html).toContain(`<input type="hidden" name="idem" value="${KEY}"/>`);
});

test("sold out with nothing left says so", async () => {
  const ev = structuredClone(event);
  ev.ticket_types[0].remaining = 0;
  const { a } = app(
    routes(
      () => apiError(409, "sold_out", { available: [{ ticket_type_id: "t1", remaining: 0 }] }),
      ev,
    ),
  );
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect(res.status).toBe(409);
  expect(await res.text()).toContain("Those tickets just sold out.");
});

test("a form we can't read is a 400", async () => {
  const { a, calls } = app(routes(created));
  const res = await a.request("/acme/gala/checkout", {
    method: "POST",
    body: "--x\r\nbroken",
    headers: { "content-type": "multipart/form-data; boundary=y" },
  });
  expect(res.status).toBe(400);
  expect(await res.text()).toContain("We couldn't read that form. Go back and try again.");
  expect(checkoutCalls(calls)).toBe(0);
});

test("each picker names its ticket", async () => {
  const { a } = app(() => Response.json(event));
  const html = await (await a.request("/acme/gala")).text();
  expect(html).toContain('<label for="qty_t1">How many General?</label>');
});

test("no quantities chosen asks for one, without trying to buy", async () => {
  const { a, calls } = app(routes(() => created()));
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "0", idem: KEY }));
  expect(res.status).toBe(422);
  expect(await res.text()).toContain("Pick at least one ticket.");
  expect(calls.every((c) => !c.url.includes("/checkouts"))).toBe(true);
});

test("sold out shows what's left next to the picker and keeps the choice", async () => {
  const ev = structuredClone(event);
  ev.ticket_types[0].remaining = 1;
  const { a } = app(
    routes(
      () => apiError(409, "sold_out", { available: [{ ticket_type_id: "t1", remaining: 1 }] }),
      ev,
    ),
  );
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect(res.status).toBe(409);
  const html = await res.text();
  expect(html).toContain("Only 1 General left.");
  expect(html).toContain('<option value="1" selected>1</option>');
  expect(html).not.toContain("API words");
});

const messages: [number, string, string][] = [
  [409, "idempotency_in_progress", "We're already holding those for you. Give it a second."],
  [409, "payments_not_ready", "This event can't take card payments just yet."],
  [503, "payments_not_configured", "This event can't take card payments just yet."],
  [503, "try_again", "Busy for a moment. Try again."],
  [503, "turnstile_unavailable", "Busy for a moment. Try again."],
  [429, "rate_limited", "That's a lot of tries. Wait a minute and try again."],
  [422, "turnstile_failed", "Finish the quick check above Get tickets, then try again."],
  [422, "sales_closed", "Sales closed on one of those tickets while you were choosing."],
  [422, "sales_not_open", "One of those tickets isn't on sale yet."],
  [422, "unknown_ticket_type", "One of those tickets isn't on sale any more."],
  [422, "quantity_out_of_range", "The per-order limit on one of those tickets changed."],
  [422, "too_many_tickets", "One order holds up to 100 tickets."],
  [500, "internal", "Something went wrong on our side. Try again."],
];

// The promo refusals sit next to the code box, where the buyer can fix them.
const codeMessages: [string, string][] = [
  ["promo_used_up", "Every use of that code has gone."],
  ["promo_invalid", "That code isn't working for this event."],
  ["promo_not_applicable", "That code doesn't take anything off these tickets."],
];

for (const [code, text] of codeMessages) {
  test(`createCheckout 422 ${code} is shown by the code box`, async () => {
    const { a } = app(routes(() => apiError(422, code)));
    const res = await a.request(
      "/acme/gala/checkout",
      form({ qty_t1: "2", code: "FRIENDS", idem: KEY }),
    );
    expect(res.status).toBe(422);
    const html = await res.text();
    const box = html.slice(html.indexOf('<form class="code"'));
    expect(box).toContain(text);
    expect(html).not.toContain("API words");
    expect(html).not.toContain("on our side");
    expect(html).toContain('<option value="2" selected>2</option>');
  });
}

test("an event that ended mid-purchase says so on the ended page", async () => {
  const ended = structuredClone(event);
  ended.event.state = "ended";
  const { a } = app(routes(() => apiError(422, "event_over"), ended));
  const res = await a.request("/acme/gala/checkout", form({ qty_t1: "2", idem: KEY }));
  expect(res.status).toBe(422);
  const html = await res.text();
  expect(html).toContain("This event finished while you were choosing");
  expect(html).not.toContain("on our side");
});

// Every 4xx code createCheckout documents gets our own words, never the 5xx catch-all.
test("no documented createCheckout refusal reads as our fault", async () => {
  const codes = [
    "sold_out",
    "payments_not_ready",
    "promo_not_applicable",
    "promo_used_up",
    "promo_invalid",
    "turnstile_failed",
    "rate_limited",
    "idempotency_in_progress",
    "sales_closed",
    "sales_not_open",
    "event_over",
    "unknown_ticket_type",
    "quantity_out_of_range",
    "too_many_tickets",
    "no_items",
  ];
  for (const code of codes) {
    const { a } = app(
      routes(() => apiError(422, code, code === "sold_out" ? { available: [] } : {})),
      {
        siteKey: "",
      },
    );
    const html = await (
      await a.request("/acme/gala/checkout", form({ qty_t1: "2", code: "X", idem: KEY }))
    ).text();
    expect([code, html.includes("on our side")]).toEqual([code, false]);
  }
});

for (const [status, code, text] of messages) {
  test(`createCheckout ${status} ${code} is shown on the event page`, async () => {
    let checkouts = 0;
    const { a } = app(
      routes(() => {
        checkouts++;
        return apiError(status, code);
      }),
    );
    const res = await a.request(
      "/acme/gala/checkout",
      form({ qty_t1: "2", code: "FRIENDS", idem: KEY }),
    );
    const html = await res.text();
    expect(checkouts).toBe(1);
    expect(html).toContain(text);
    expect(html).not.toContain("API words");
    expect(html).toContain("Spring Gala");
    expect(html).toContain('<option value="2" selected>2</option>');
    expect(html).toContain("cf-turnstile");
  });
}

test("/favicon.ico answers 204, so browsers log no 404", async () => {
  const { a } = app(() => new Response(null, { status: 500 }));
  const res = await a.request("/favicon.ico");
  expect(res.status).toBe(204);
});

test("the API being down is a plain error page", async () => {
  const { a } = app(() => {
    throw new Error("down");
  });
  const res = await a.request("/acme/gala");
  expect(res.status).toBe(502);
  expect(await res.text()).toContain("Something went wrong on our side. Try again.");
});

test("security headers on every response", async () => {
  const { a } = app(routes(created));
  // A timezone Intl doesn't know throws while rendering: our bug, so a 500.
  const bad = structuredClone(event);
  bad.event.timezone = "Nowhere/Special";
  const { a: broken } = app(() => Response.json(bad));
  const responses = [
    ...(await Promise.all(["/acme/gala", "/nope", "/assets/missing.css"].map((p) => a.request(p)))),
    await a.request("/acme/gala/checkout", form({ qty_t1: "1", idem: KEY })),
    await a.request("/acme/gala/checkout", form({ pad: "x".repeat(70 * 1024) })),
    await broken.request("/acme/gala"),
  ];
  expect(responses.slice(3).map((r) => r.status)).toEqual([303, 413, 500]);
  expect(await responses[5].text()).toContain("Something went wrong on our side. Try again.");
  for (const res of responses) {
    expect(res.headers.get("x-content-type-options")).toBe("nosniff");
    expect(res.headers.get("referrer-policy")).toBe("strict-origin-when-cross-origin");
    expect(res.headers.get("x-frame-options")).toBe("DENY");
    expect(res.headers.get("content-security-policy")).toContain("default-src 'self'");
  }
});

test("X-Real-IP counts only from a loopback peer", async () => {
  const send = async (peer: string, headers: Record<string, string>) => {
    const { a, calls } = app(() => Response.json(event), { peer });
    await a.request("/acme/gala", { headers });
    return calls[0].headers.get("x-real-ip");
  };
  expect(await send("127.0.0.1", { "x-real-ip": "203.0.113.9" })).toBe("203.0.113.9");
  expect(await send("::1", { "x-real-ip": "203.0.113.9" })).toBe("203.0.113.9");
  expect(await send("198.51.100.7", { "x-real-ip": "203.0.113.9" })).toBe("198.51.100.7");
  expect(await send("127.0.0.1", {})).toBe("127.0.0.1");
  // Only a single address counts; anything else falls back to the peer.
  expect(await send("127.0.0.1", { "x-real-ip": "203.0.113.9, 10.0.0.1" })).toBe("127.0.0.1");
  expect(await send("127.0.0.1", { "x-real-ip": "not-an-ip" })).toBe("127.0.0.1");
});

test("only the buyer's language and browser reach the API", async () => {
  const { a, calls } = app(() => Response.json(event));
  await a.request("/acme/gala", {
    headers: {
      "accept-language": "en-US",
      "user-agent": "UA",
      cookie: "a=b",
      authorization: "Bearer x",
      "x-forwarded-for": "1.2.3.4",
    },
  });
  const h = calls[0].headers;
  expect(h.get("accept-language")).toBe("en-US");
  expect(h.get("user-agent")).toBe("UA");
  expect(h.get("cookie")).toBeNull();
  expect(h.get("authorization")).toBeNull();
  expect(h.get("x-forwarded-for")).toBeNull();
});

test("big bodies are refused", async () => {
  const { a, calls } = app(() => Response.json(event));
  const res = await a.request(
    "/acme/gala/checkout",
    form({ qty_t1: "1", pad: "x".repeat(70 * 1024) }),
  );
  expect(res.status).toBe(413);
  expect(calls).toHaveLength(0);
});

// --- Checkout and order pages ---

const checkoutEvent = {
  organizer: "acme",
  slug: "gala",
  name: "Gala </script><script>alert(1)</script>",
  timezone: "America/Denver",
  ticket_types: [{ id: "t1", name: "General" }],
  questions: [
    {
      id: "q1",
      label: "Dietary needs",
      kind: "text",
      options: [],
      required: true,
      scope: "order",
      applies_to_all: true,
      applies_to: [],
    },
  ],
};
const checkout = (over: Record<string, unknown> = {}) => ({
  id: "c1",
  event_id: "e1",
  status: "open",
  expires_at: "2027-05-02T01:15:00Z",
  items: [{ ticket_type_id: "t1", quantity: 2 }],
  promo_code: "FRIENDS",
  quote: {
    tickets: [1, 2].map(() => ({
      ticket_type_id: "t1",
      price_cents: 2500,
      all_in_cents: 2600,
      discount_cents: 250,
      tax_cents: 0,
      fee_cents: 95,
      charge_cents: 2250,
      tax_included: false,
    })),
    subtotal_cents: 5000,
    discount_cents: 500,
    all_in_discount_cents: 510,
    tax_cents: 0,
    fee_cents: 190,
    total_cents: 4690,
    included_tax_cents: 0,
    fee_mode: "pass",
  },
  event: checkoutEvent,
  ...over,
});
const ORDER_URL = `http://localhost:3003/orders/TS-7K2QF?t=${TOK}`;

test("the checkout page carries its data, the island and Stripe's policy", async () => {
  const { a, calls } = app(() => Response.json(checkout()));
  const res = await a.request(`/checkout/${TOK}`, { headers: { "x-real-ip": "203.0.113.9" } });
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(new URL(calls[0].url).pathname).toBe(`/v1/checkouts/${TOK}`);
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(res.headers.get("cache-control")).toBe("no-store");
  expect(res.headers.get("referrer-policy")).toBe("no-referrer");
  const policy = res.headers.get("content-security-policy") ?? "";
  expect(policy).toContain("script-src 'self' https://js.stripe.com");
  expect(policy).toContain("https://hooks.stripe.com");
  expect(policy).toContain("connect-src 'self' https://api.stripe.com");
  expect(policy).not.toContain("cloudflare");
  expect(html).toContain('<script type="module" src="/assets/checkout.js"></script>');
  const block = html.match(/<script type="application\/json" id="checkout-data">(.*?)<\/script>/s);
  expect(block).not.toBeNull();
  // The event's name can't end the block early.
  expect(block?.[1]).not.toContain("<");
  const data = JSON.parse(block?.[1] ?? "");
  expect(data).toMatchObject({
    token: TOK,
    expires_at: "2027-05-02T01:15:00Z",
    items: [{ ticket_type_id: "t1", quantity: 2 }],
    ticket_names: { t1: "General" },
    event_name: checkoutEvent.name,
    timezone: "America/Denver",
    organizer: "acme",
    event_slug: "gala",
  });
  expect(data.questions).toHaveLength(1);
  expect(data.quote.total_cents).toBe(4690);
  // The quote is on the page before any script runs.
  expect(html).toContain("2 × General");
  expect(html).toContain("$52.00");
  expect(html).toContain("−$5.10");
  expect(html).toContain("$46.90");
  expect(html).toContain("Includes $1.90 service fee");
  expect(html).not.toContain("Service fee</span>");
  expect(html).not.toMatch(/style="/);
});

test("a completed checkout goes to its order", async () => {
  const { a } = app(() =>
    Response.json(checkout({ status: "completed", order_code: "TS-7K2QF", order_url: ORDER_URL })),
  );
  const res = await a.request(`/checkout/${TOK}`);
  expect(res.status).toBe(303);
  expect(res.headers.get("location")).toBe(`/orders/TS-7K2QF?t=${TOK}`);
  expect(res.headers.get("cache-control")).toBe("no-store");
});

test("a completed checkout whose link was replaced points to the email", async () => {
  const { a } = app(() => Response.json(checkout({ status: "completed", order_code: "TS-7K2QF" })));
  const res = await a.request(`/checkout/${TOK}`);
  expect(res.status).toBe(200);
  expect(res.headers.get("location")).toBeNull();
  const html = await res.text();
  expect(html).toContain(
    "You've got these tickets. To see them, open the newest link we emailed you.",
  );
  expect(html).not.toContain("<script");
});

test("an expired checkout says so and links back, with no script", async () => {
  const { a } = app(() => Response.json(checkout({ status: "expired" })));
  const res = await a.request(`/checkout/${TOK}`);
  const html = await res.text();
  expect(res.status).toBe(410);
  expect(html).toContain("Your 15 minutes ran out and the tickets went back on sale.");
  expect(html).toContain('href="/acme/gala"');
  expect(html).not.toContain("checkout.js");
  expect(res.headers.get("content-security-policy")).not.toContain("stripe");
});

test("a checkout whose payment was stopped says so and links back, with no script", async () => {
  const { a } = app(() => Response.json(checkout({ status: "cancelled" })));
  const res = await a.request(`/checkout/${TOK}`);
  const html = await res.text();
  expect(res.status).toBe(410);
  expect(html).toContain("The tickets you were holding have gone back on sale.");
  expect(html).not.toContain("may still");
  expect(html).toContain('href="/acme/gala"');
  expect(html).not.toContain("checkout.js");
});

test("a cancelled event's checkout says the event is cancelled", async () => {
  const { a } = app(() =>
    Response.json({ error: { code: "event_cancelled", message: "m" } }, { status: 409 }),
  );
  const res = await a.request(`/checkout/${TOK}`);
  const html = await res.text();
  expect(res.status).toBe(409);
  expect(html).toContain("This event has been cancelled, so it");
  expect(html).toContain("refunding you in full");
});

test("a paying checkout past its hold keeps the island, never the expired page", async () => {
  // The 3D Secure return_url lands here after the hold ran out, with a card maybe charged.
  const { a } = app(() =>
    Response.json(checkout({ status: "paying", expires_at: "2020-01-01T00:00:00Z" })),
  );
  const res = await a.request(`/checkout/${TOK}?payment_intent=pi_1&redirect_status=succeeded`);
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).not.toContain("back on sale");
  expect(html).toContain("checkout.js");
  const block = html.match(/<script type="application\/json" id="checkout-data">(.*?)<\/script>/s);
  const data = JSON.parse(block?.[1] ?? "");
  expect(data.status).toBe("paying");
  // The id names what the island keeps across the bank's redirect, so the token never has to.
  expect(data.id).toBe(checkout({}).id);
});

test("an unknown or malformed checkout token is the 404 page", async () => {
  const { a, calls } = app(() => apiError(404, "not_found"));
  expect((await a.request(`/checkout/${TOK}`)).status).toBe(404);
  expect((await a.request("/checkout/tok")).status).toBe(404);
  expect(calls).toHaveLength(1);
});

const payReq = (
  key: string | undefined,
  body = '{"email":"ada@example.com"}',
  type = "application/json",
) => ({
  method: "POST",
  body,
  headers: {
    "content-type": type,
    "x-real-ip": "203.0.113.9",
    cookie: "a=b",
    ...(key ? { "idempotency-key": key } : {}),
  },
});

test("pay is passed through with the key, the buyer's address and nothing else", async () => {
  const { a, calls } = app(() =>
    Response.json({ kind: "test", client_secret: "pi_1_secret", amount_cents: 4690 }),
  );
  const res = await a.request(`/api/checkouts/${TOK}/pay`, payReq(KEY));
  expect(res.status).toBe(200);
  expect(res.headers.get("cache-control")).toBe("no-store");
  expect(await res.json()).toMatchObject({ kind: "test", amount_cents: 4690 });
  const call = calls[0];
  expect(call.method).toBe("POST");
  expect(new URL(call.url).pathname).toBe(`/v1/checkouts/${TOK}/pay`);
  expect(call.headers.get("idempotency-key")).toBe(KEY);
  expect(call.headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(call.headers.get("cookie")).toBeNull();
  expect(await call.json()).toEqual({ email: "ada@example.com" });
});

test("pay errors keep their status, code and Retry-After", async () => {
  const { a } = app(
    () =>
      new Response(JSON.stringify({ error: { code: "idempotency_in_progress", message: "m" } }), {
        status: 409,
        headers: { "content-type": "application/json", "retry-after": "1" },
      }),
  );
  const res = await a.request(`/api/checkouts/${TOK}/pay`, payReq(KEY));
  expect(res.status).toBe(409);
  expect(res.headers.get("retry-after")).toBe("1");
  expect((await res.json()).error.code).toBe("idempotency_in_progress");
});

test("pay refuses a bad token, key, type or body without asking the API", async () => {
  const { a, calls } = app(() => Response.json({}));
  const statuses = await Promise.all([
    a.request("/api/checkouts/tok/pay", payReq(KEY)),
    a.request(`/api/checkouts/${TOK}/pay`, payReq(undefined)),
    a.request(`/api/checkouts/${TOK}/pay`, payReq("k-1")),
    a.request(
      `/api/checkouts/${TOK}/pay`,
      payReq(KEY, "email=a", "application/x-www-form-urlencoded"),
    ),
    a.request(`/api/checkouts/${TOK}/pay`, payReq(KEY, "{not json")),
  ]).then((rs) => rs.map((r) => r.status));
  expect(statuses).toEqual([404, 400, 400, 415, 400]);
  expect(calls).toHaveLength(0);
});

test("the island reads the checkout through the same origin", async () => {
  const { a, calls } = app(() => Response.json(checkout({ status: "paying" })));
  const res = await a.request(`/api/checkouts/${TOK}`, { headers: { "x-real-ip": "203.0.113.9" } });
  expect(res.status).toBe(200);
  expect((await res.json()).status).toBe("paying");
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");
  expect((await a.request("/api/checkouts/tok")).status).toBe(404);
  expect(calls).toHaveLength(1);
});

const order = {
  code: "TS-7K2QF",
  status: "paid",
  email: "ada@example.com",
  refunded_cents: 0,
  returned_cents: 0,
  event: {
    status: "live",
    name: "Spring Gala",
    starts_at: "2027-05-02T01:00:00Z",
    ends_at: "2027-05-02T05:00:00Z",
    timezone: "America/Denver",
    venue_name: "The Hall",
    venue_address: "",
    organizer_name: "Acme",
  },
  totals: {
    subtotal_cents: 5000,
    discount_cents: 500,
    tax_cents: 0,
    fee_cents: 190,
    total_cents: 4690,
  },
  tickets: [
    {
      id: "k1",
      ticket_type_name: "General",
      attendee_name: "Ada",
      status: "valid",
      refunded: false,
      qr: "x".repeat(22) as string | null,
      qr_png:
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGNgAAAAAgABSK+kcQAAAABJRU5ErkJggg==" as
          | string
          | null,
    },
    {
      id: "k2",
      ticket_type_name: "General",
      attendee_name: "<b>Grace</b>",
      status: "valid",
      refunded: false,
      qr: "y".repeat(22) as string | null,
      qr_png:
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGP4DwABAQEAWr0wvAAAAABJRU5ErkJggg==" as
          | string
          | null,
    },
  ],
};

test("the order page lists each ticket and says to keep the link private", async () => {
  const { a, calls } = app(() => Response.json(order));
  const res = await a.request(`/orders/TS-7K2QF?t=${TOK}`, {
    headers: { "x-real-ip": "203.0.113.9" },
  });
  const html = await res.text();
  expect(res.status).toBe(200);
  const url = new URL(calls[0].url);
  expect(url.pathname).toBe("/v1/orders/TS-7K2QF");
  expect(url.searchParams.get("token")).toBe(TOK);
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(res.headers.get("cache-control")).toBe("no-store");
  expect(res.headers.get("content-security-policy")).not.toContain("script-src");
  expect(html).not.toContain("<script");
  expect(html).toContain("Ticket 1 of 2: General");
  expect(html).toContain("Ada");
  expect(html).toContain("&lt;b>Grace&lt;/b>");
  expect(html).toContain("Keep this link to yourself: anyone with it can see your tickets.");
  // The QR values aren't shown until there's a code to scan.
  expect(html).not.toContain("x".repeat(22));
});

test("an order link that doesn't work is a 404 that says to check the email", async () => {
  const { a, calls } = app(() => apiError(404, "not_found"));
  for (const path of [`/orders/TS-7K2QF?t=${TOK}`, "/orders/TS-7K2QF", `/orders/nope?t=${TOK}`]) {
    const res = await a.request(path);
    expect(res.status).toBe(404);
    expect(await res.text()).toContain(
      "That link doesn't open an order. Check your email for the latest one.",
    );
  }
  expect(calls).toHaveLength(1);
});

// A token in the address must not ride out on Referer, even to our own /assets/storefront.css:
// every answer on a token path, errors and redirects included, ends with no-referrer.
test("token pages send no Referer, whatever the answer", async () => {
  const done = checkout({ status: "completed", order_code: "TS-7K2QF", order_url: ORDER_URL });
  const cases: [Api, string][] = [
    [() => Response.json(order), `/orders/TS-7K2QF?t=${TOK}`],
    [() => apiError(404, "not_found"), `/orders/TS-7K2QF?t=${TOK}`],
    [() => apiError(429, "rate_limited"), `/orders/TS-7K2QF?t=${TOK}`],
    [() => Response.json(checkout()), `/checkout/${TOK}`],
    [() => Response.json(checkout({ status: "expired" })), `/checkout/${TOK}`],
    [() => Response.json(done), `/checkout/${TOK}`],
    [() => apiError(404, "not_found"), `/checkout/${TOK}`],
    [() => Response.json(checkout()), `/api/checkouts/${TOK}`],
  ];
  for (const [api, path] of cases) {
    const res = await app(api).a.request(path);
    expect([path, res.status, res.headers.get("referrer-policy")]).toEqual([
      path,
      res.status,
      "no-referrer",
    ]);
  }
  // Pages without a token keep the site's usual policy.
  const ev = await app(() => Response.json(event)).a.request("/acme/gala");
  expect(ev.headers.get("referrer-policy")).toBe("strict-origin-when-cross-origin");
});

test("the health check answers without the API", async () => {
  const { a, calls } = app(() => Response.json({}));
  const res = await a.request("/healthz");
  expect(res.status).toBe(200);
  expect(calls).toHaveLength(0);
});

test("a completed checkout's link can't send the buyer to another site", async () => {
  for (const order_url of [
    `http://localhost:3003//evil.example/orders/TS-7K2QF?t=${TOK}`,
    `http://localhost:3003/\\evil.example/orders/TS-7K2QF?t=${TOK}`,
  ]) {
    const { a } = app(() =>
      Response.json(checkout({ status: "completed", order_code: "TS-7K2QF", order_url })),
    );
    const res = await a.request(`/checkout/${TOK}`);
    const to = res.headers.get("location");
    if (res.status === 303) expect(to).toBe(`/orders/TS-7K2QF?t=${TOK}`);
    else expect(res.status).toBe(502);
    expect(to ?? "").not.toContain("evil");
  }
  const { a } = app(() =>
    Response.json(
      checkout({ status: "completed", order_code: "TS-7K2QF", order_url: "/orders/x?t=y" }),
    ),
  );
  expect((await a.request(`/checkout/${TOK}`)).status).toBe(502);
});

test("too many checkout reads is the slow-down page", async () => {
  const { a } = app(() => apiError(429, "rate_limited"));
  const res = await a.request(`/checkout/${TOK}`);
  expect(res.status).toBe(429);
  expect(await res.text()).toContain("That's a lot of tries. Wait a minute and try again.");
});

// The visible lines add up to the total, whether tax is on top or already in the price.
const sumsUp = (html: string) => {
  const quote = html.match(/<ul class="quote">(.*?)<\/ul>/s)?.[1] ?? "";
  const cents = (s: string) => Number(s.replace(/[^0-9]/g, ""));
  const rows = [
    ...quote.matchAll(/<li( class="total")?><span>[^<]*<\/span><span>([^<]*)<\/span><\/li>/g),
  ];
  const total = rows.find((r) => r[1]);
  const sum = rows
    .filter((r) => !r[1])
    .reduce((n, r) => n + (r[2].startsWith("−") ? -cents(r[2]) : cents(r[2])), 0);
  return { sum, total: cents(total?.[2] ?? "") };
};

const taxed = (inclusive: boolean, allIn = true) => {
  // $25 at 8.25%: inclusive keeps $25.00 with $1.91 inside; exclusive adds $2.06.
  const t = inclusive
    ? {
        price_cents: 2500,
        discount_cents: 0,
        tax_cents: 191,
        fee_cents: 96,
        charge_cents: 2500,
        tax_included: true,
      }
    : {
        price_cents: 2500,
        discount_cents: 0,
        tax_cents: 206,
        fee_cents: 100,
        charge_cents: 2706,
        tax_included: false,
      };
  return checkout({
    items: [{ ticket_type_id: "t1", quantity: 1 }],
    quote: {
      tickets: [
        {
          ticket_type_id: "t1",
          ...t,
          ...(allIn ? { all_in_cents: t.charge_cents + t.fee_cents } : {}),
        },
      ],
      subtotal_cents: 2500,
      discount_cents: 0,
      ...(allIn ? { all_in_discount_cents: 0 } : {}),
      tax_cents: t.tax_cents,
      fee_cents: t.fee_cents,
      total_cents: t.charge_cents + t.fee_cents,
      included_tax_cents: t.tax_included ? t.tax_cents : 0,
      fee_mode: "pass",
    },
  });
};

test("tax on top is a note under the total, with the fee, and no Tax line", async () => {
  const { a } = app(() => Response.json(taxed(false)));
  const html = await (await a.request(`/checkout/${TOK}`)).text();
  expect(html).not.toContain("<span>Tax</span>");
  expect(html).not.toContain("Service fee</span>");
  expect(html).toContain("Includes $2.06 tax");
  expect(html).toContain("Includes $1.00 service fee");
  expect(sumsUp(html)).toEqual({ sum: 2806, total: 2806 });
});

test("tax inside the price is noted under the total too", async () => {
  const { a } = app(() => Response.json(taxed(true)));
  const html = await (await a.request(`/checkout/${TOK}`)).text();
  expect(html).not.toContain("<span>Tax</span>");
  expect(html).toContain("Includes $1.91 tax");
  expect(sumsUp(html)).toEqual({ sum: 2596, total: 2596 });
});

test("no tax and no fee means no notes", async () => {
  const c = taxed(true);
  const body = { ...c, quote: { ...c.quote, tax_cents: 0, fee_cents: 0 } };
  const html = await (await app(() => Response.json(body)).a.request(`/checkout/${TOK}`)).text();
  expect(html).not.toContain("Includes");
});

test("an API without all-in prices still gets the old quote lines, never $NaN", async () => {
  const { a } = app(() => Response.json(taxed(false, false)));
  const html = await (await a.request(`/checkout/${TOK}`)).text();
  expect(html).toContain("<span>Tax</span><span>$2.06</span>");
  expect(html).toContain("<span>Service fee</span><span>$1.00</span>");
  expect(html).not.toMatch(/NaN|undefined/);
  expect(sumsUp(html)).toEqual({ sum: 2806, total: 2806 });
});

test("every price on the event page is all-in", async () => {
  const base = event.ticket_types[0];
  const ev = structuredClone(event);
  ev.ticket_types = [
    {
      ...base,
      id: "a",
      name: "Pass",
      price_cents: 2500,
      all_in_cents: 2600,
      all_in_fee_cents: 100,
      all_in_tax_cents: 0,
    },
    {
      ...base,
      id: "b",
      name: "Taxed",
      price_cents: 2000,
      all_in_cents: 2255,
      all_in_fee_cents: 90,
      all_in_tax_cents: 165,
    },
    {
      ...base,
      id: "c",
      name: "Absorbed",
      price_cents: 1000,
      all_in_cents: 1000,
      all_in_fee_cents: 0,
      all_in_tax_cents: 0,
    },
    {
      ...base,
      id: "d",
      name: "Free",
      price_cents: 0,
      all_in_cents: 0,
      all_in_fee_cents: 0,
      all_in_tax_cents: 0,
    },
  ];
  const html = await (
    await app(() => Response.json(ev), { siteKey: "" }).a.request("/acme/gala")
  ).text();
  const allowed = new Set(["$26.00", "$1.00", "$22.55", "$0.90", "$1.65", "$10.00"]);
  const found = html.match(/\$[\d,]+\.\d\d/g) ?? [];
  expect(found.length).toBeGreaterThan(0);
  for (const m of found) expect(allowed.has(m)).toBe(true);
  expect(html).not.toContain("$25.00");
  expect(html).not.toContain("$20.00");
  expect(html).not.toContain("A service fee is added at checkout.");
  expect(html).toContain("Includes $1.00 service fee");
  expect(html).toContain("Includes $0.90 service fee and $1.65 tax");
  expect(html).toContain("Free");
});

test("an API without all-in prices shows the old price and fee note, never $NaN", async () => {
  const ev = structuredClone(event);
  const { all_in_cents, all_in_fee_cents, all_in_tax_cents, ...old } = ev.ticket_types[0];
  // @ts-expect-error an older API's ticket type
  ev.ticket_types = [old];
  const html = await (
    await app(() => Response.json(ev), { siteKey: "" }).a.request("/acme/gala")
  ).text();
  expect(html).toContain("$25.00");
  expect(html).toContain("A service fee is added at checkout.");
  expect(html).not.toMatch(/NaN|undefined/);
});

// An error can carry the URL we fetched, token and all; the log gets its kind and a redacted message.
test("a crash logs no token from the error's message", async () => {
  const bad = structuredClone(event);
  bad.event.timezone = `http://api/v1/checkouts/${TOK}/pay?t=${TOK}`;
  const { a } = app(() => Response.json(bad));
  const spy = spyOn(console, "error").mockImplementation(() => {});
  try {
    const res = await a.request("/acme/gala");
    expect(res.status).toBe(500);
    const logged = spy.mock.calls.map((c) => c.map(String).join(" ")).join("\n");
    expect(logged).toContain("RangeError");
    expect(logged).not.toContain(TOK);
  } finally {
    spy.mockRestore();
  }
});

test("the order page says what was refunded, from the refund rows", async () => {
  const show = async (o: unknown) => {
    const { a } = app(() => Response.json(o));
    return (await a.request(`/orders/TS-7K2QF?t=${TOK}`)).text();
  };
  const part = structuredClone(order);
  part.status = "partially_refunded";
  part.refunded_cents = 2345;
  part.tickets[0] = { ...part.tickets[0], status: "void", refunded: true, qr: null, qr_png: null };
  let html = await show(part);
  expect(html).toContain(
    "Part of this order was refunded: $23.45 is on its way back to your card.",
  );
  expect(html).toContain("Refunded");
  expect(html).not.toContain("x".repeat(22));
  // The one ticket left is ticket 1 of 1, as in the email and the PDF.
  expect(html.match(/<img /g)).toHaveLength(1);
  expect(html).toContain('alt="QR code for ticket 1"');
  expect(html).toContain("Ticket 1 of 1: General");
  expect(html).not.toContain("<svg");

  const full = structuredClone(order);
  full.status = "refunded";
  full.refunded_cents = 4690;
  full.tickets = full.tickets.map((t) => ({
    ...t,
    status: "void",
    refunded: true,
    qr: null,
    qr_png: null,
  }));
  html = await show(full);
  expect(html).toContain(
    "This order was refunded in full: $46.90 is on its way back to your card.",
  );

  // Every ticket refunded, our fee kept: not "part of this order".
  const kept = structuredClone(full);
  kept.refunded_cents = 4500;
  html = await show(kept);
  expect(html).toContain(
    "You're getting everything back except the $1.90 service fee: $45.00 is on its way to your card.",
  );
  expect(html).not.toContain("Part of this order");

  full.event.status = "cancelled";
  html = await show(full);
  expect(html).toContain(
    "Spring Gala was cancelled, so this order was refunded in full: $46.90 is on its way back to your card.",
  );
  expect(html).not.toContain("arrived after the last tickets");

  // A late payment on an event that was cancelled meanwhile is the cancellation's doing.
  const late = structuredClone(order);
  late.status = "cancelled";
  late.refunded_cents = 4690;
  late.tickets = [];
  late.event.status = "cancelled";
  html = await show(late);
  expect(html).toContain("Spring Gala was cancelled while your payment was on its way");
  expect(html).not.toContain("arrived after the last tickets");

  // A free order voided, by the organizer or by the event's cancellation: no money moved.
  const zero = structuredClone(order);
  zero.status = "cancelled";
  zero.totals = {
    subtotal_cents: 0,
    discount_cents: 0,
    tax_cents: 0,
    fee_cents: 0,
    total_cents: 0,
  };
  zero.tickets = zero.tickets.map((t) => ({
    ...t,
    status: "void",
    refunded: false,
    qr: null,
    qr_png: null,
  }));
  html = await show(zero);
  expect(html).toContain("These tickets were cancelled.");
  expect(html).not.toContain("refunding");
  zero.event.status = "cancelled";
  html = await show(zero);
  expect(html).toContain("Spring Gala was cancelled, so these tickets won't get you in any more.");
  expect(html).not.toContain("refunding");

  // Voided without a refund (a free ticket) reads Cancelled.
  const free = structuredClone(order);
  free.tickets[1] = { ...free.tickets[1], status: "void", refunded: false, qr: null, qr_png: null };
  html = await show(free);
  expect(html).toContain("Cancelled");
  expect(html).toContain("You're going.");
});

test("a refund the bank sent back tells the buyer to contact the organizer", async () => {
  const o = structuredClone(order);
  o.status = "refunded";
  o.returned_cents = 4690;
  o.tickets = o.tickets.map((t) => ({
    ...t,
    status: "void",
    refunded: true,
    qr: null,
    qr_png: null,
  }));
  const { a } = app(() => Response.json(o));
  const html = await (await a.request(`/orders/TS-7K2QF?t=${TOK}`)).text();
  expect(html).toContain("Your bank sent $46.90 of your refund back, so it's with Acme again.");
  expect(html).not.toContain("refunded in full");
  expect(html).not.toContain('class="order-status"');
});

test("every event page links to getting the ticket link again", async () => {
  const ended = structuredClone(event);
  ended.event.state = "ended";
  ended.ticket_types[0].on_sale = false;
  const soldOut = structuredClone(event);
  soldOut.ticket_types[0].remaining = 0;
  for (const ev of [event, ended, soldOut]) {
    const { a } = app(() => Response.json(ev));
    const html = await (await a.request("/acme/gala")).text();
    expect(html).toContain(
      '<a href="/acme/gala/order-link">Already bought? Get your ticket link again</a>',
    );
  }
});

const linkRoutes =
  (answer: () => Response): Api =>
  (r) =>
    new URL(r.url).pathname.endsWith("/order-links") ? answer() : Response.json(event);

test("the order link page asks for an email behind the quick check", async () => {
  const { a } = app(linkRoutes(() => new Response(null, { status: 204 })));
  const res = await a.request("/acme/gala/order-link");
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).toContain("Get your ticket link again");
  expect(html).toContain(
    "Tell us the email you bought with. If it has tickets for this event, we'll send a fresh link there, and the old one will stop working.",
  );
  expect(html).toContain('action="/acme/gala/order-link"');
  expect(html).toContain('type="email"');
  expect(html).toContain('data-sitekey="1x00000000000000000000AA"');
  expect(html).toContain("Send my link");
  expect(res.headers.get("content-security-policy")).toContain(
    "frame-src https://challenges.cloudflare.com",
  );
  // Turnstile itself waits for the buyer, as on the event page.
  expect(html).not.toContain("challenges.cloudflare.com/turnstile");
  expect(html).toContain('<script type="module" src="/assets/turnstile.js">');
});

test("asking for the link again says the same thing for any address", async () => {
  const { a, calls } = app(linkRoutes(() => new Response(null, { status: 204 })));
  const pages: string[] = [];
  for (const email of ["ada@example.com", "nobody@example.com"]) {
    const res = await a.request(
      "/acme/gala/order-link",
      form({ email, "cf-turnstile-response": "tok-1" }),
    );
    expect(res.status).toBe(200);
    pages.push(await res.text());
  }
  expect(pages[0]).toBe(pages[1]);
  expect(pages[0]).toContain(
    "Done. If that address bought tickets for Spring Gala, the link is on its way. It can take a minute or two.",
  );
  const posts = calls.filter(
    (c) => new URL(c.url).pathname === "/v1/storefront/acme/gala/order-links",
  );
  expect(posts).toHaveLength(2);
  expect(await posts[0].json()).toEqual({ email: "ada@example.com", turnstile_token: "tok-1" });
  expect(posts[0].headers.get("x-real-ip")).toBe("203.0.113.9");
  expect(posts[0].headers.get("idempotency-key")).toMatch(/^[0-9a-f-]{36}$/);
  expect(posts[1].headers.get("idempotency-key")).not.toBe(posts[0].headers.get("idempotency-key"));
});

test("the order link page shows our words for each refusal", async () => {
  const cases: [Response, number, string][] = [
    [apiError(422, "bad_email", { field: "email" }), 422, "That email doesn't look right."],
    [
      apiError(429, "rate_limited"),
      429,
      "That's a lot of tries from here. Wait a while and try again.",
    ],
    [apiError(422, "turnstile_failed"), 422, "We couldn't check you're a person. Try again."],
    [apiError(400, "bad_request"), 400, "Something went wrong on our side. Try again."],
  ];
  for (const [answer, status, words] of cases) {
    const { a } = app(linkRoutes(() => answer.clone()));
    const res = await a.request("/acme/gala/order-link", form({ email: "ada@example" }));
    const html = await res.text();
    expect(res.status).toBe(status);
    expect(html).toContain(words);
    expect(html).not.toContain("API words");
    expect(html).toContain('value="ada@example"');
  }
});

const QR_ONE = order.tickets[0].qr_png;
const QR_TWO = order.tickets[1].qr_png;

test("the order page shows each valid ticket's QR code, the PDF and the email button", async () => {
  const { a } = app(() => Response.json(order));
  const res = await a.request(`/orders/TS-7K2QF?t=${TOK}`);
  const html = await res.text();
  expect(html.match(/<img class="qr"/g)).toHaveLength(2);
  expect(html).toContain(
    `<img class="qr" src="data:image/png;base64,${QR_ONE}" alt="QR code for ticket 1" width="264" height="264"/>`,
  );
  expect(html).toContain(`src="data:image/png;base64,${QR_TWO}" alt="QR code for ticket 2"`);
  expect(html).toContain(
    `<a href="/orders/TS-7K2QF/tickets.pdf?t=${TOK}">Download all tickets (PDF)</a>`,
  );
  expect(html).toContain('<form method="post" action="/orders/TS-7K2QF/email"');
  expect(html).toContain(`<input type="hidden" name="t" value="${TOK}"/>`);
  expect(html).toContain("Email me my tickets again");
  expect(res.headers.get("content-security-policy")).toContain("img-src 'self' data:");
  expect(res.headers.get("content-security-policy")).not.toContain("script-src");
  // Only the order page takes data: images.
  const ev = await app(() => Response.json(event)).a.request("/acme/gala");
  expect(ev.headers.get("content-security-policy")).not.toContain("data:");
  const missing = await app(() => apiError(404, "not_found")).a.request(
    `/orders/TS-7K2QF?t=${TOK}`,
  );
  expect(missing.headers.get("content-security-policy")).not.toContain("data:");
});

test("an order with no valid ticket has no QR, no PDF and no email button", async () => {
  const o = structuredClone(order);
  o.status = "refunded";
  o.refunded_cents = 4690;
  o.tickets = o.tickets.map((t) => ({
    ...t,
    status: "void",
    refunded: true,
    qr: null,
    qr_png: null,
  }));
  const { a } = app(() => Response.json(o));
  const html = await (await a.request(`/orders/TS-7K2QF?t=${TOK}`)).text();
  expect(html).not.toContain('class="qr"');
  expect(html).not.toContain("QR code for ticket");
  expect(html).not.toContain("tickets.pdf");
  expect(html).not.toContain("Download all tickets (PDF)");
  expect(html).not.toContain("<form");
  expect(html).not.toContain("Email me my tickets again");
});

test("the PDF link passes the API's PDF through, privately", async () => {
  const pdf = "%PDF-1.7 fake";
  const { a, calls } = app(
    () =>
      new Response(pdf, {
        headers: {
          "content-type": "application/pdf",
          "content-length": String(pdf.length),
          "content-disposition": 'attachment; filename="tickets-TS-7K2QF.pdf"',
          "cache-control": "no-store",
        },
      }),
  );
  const res = await a.request(`/orders/TS-7K2QF/tickets.pdf?t=${TOK}`, {
    headers: { "x-real-ip": "203.0.113.9" },
  });
  expect(res.status).toBe(200);
  expect(res.headers.get("content-type")).toBe("application/pdf");
  expect(res.headers.get("content-disposition")).toBe(
    'attachment; filename="tickets-TS-7K2QF.pdf"',
  );
  expect(res.headers.get("cache-control")).toBe("no-store");
  expect(res.headers.get("referrer-policy")).toBe("no-referrer");
  expect(res.headers.get("content-length")).toBe(String(pdf.length));
  expect(await res.text()).toBe(pdf);
  const url = new URL(calls[0].url);
  expect(url.pathname).toBe("/v1/orders/TS-7K2QF/tickets.pdf");
  expect(url.searchParams.get("token")).toBe(TOK);
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");

  for (const [api, path] of [
    [() => apiError(404, "not_found"), `/orders/TS-7K2QF/tickets.pdf?t=${TOK}`],
    [() => Response.json({}), "/orders/TS-7K2QF/tickets.pdf"],
  ] as [Api, string][]) {
    const r = await app(api).a.request(path);
    expect(r.status).toBe(404);
    expect(r.headers.get("content-type")).toContain("text/html");
    expect(r.headers.get("referrer-policy")).toBe("no-referrer");
    expect(await r.text()).toContain("That link doesn't open an order.");
  }

  // No valid ticket left: back to the order page, which says why.
  const none = await app(() => apiError(409, "no_tickets")).a.request(
    `/orders/TS-7K2QF/tickets.pdf?t=${TOK}`,
  );
  expect(none.status).toBe(303);
  expect(none.headers.get("location")).toBe(`/orders/TS-7K2QF?t=${TOK}`);

  // No length, or more than 20 MB: refused, and none of the body goes out.
  for (const length of [null, String(20 * 1024 * 1024 + 1)]) {
    const headers: Record<string, string> = { "content-type": "application/pdf" };
    if (length) headers["content-length"] = length;
    const body = new ReadableStream({
      start(ctl) {
        ctl.enqueue(new TextEncoder().encode(pdf));
        ctl.close();
      },
    });
    const r = await app(() => new Response(body, { headers })).a.request(
      `/orders/TS-7K2QF/tickets.pdf?t=${TOK}`,
    );
    expect(r.status).toBe(502);
    expect(r.headers.get("content-type")).toContain("text/html");
    expect(await r.text()).not.toContain(pdf);
  }
});

test("asking for the email again answers on the order page", async () => {
  const ask = async (api: Api) => {
    const { a, calls } = app(api);
    const res = await a.request("/orders/TS-7K2QF/email", form({ t: TOK }));
    return { res, calls };
  };
  const { res, calls } = await ask(() => new Response(null, { status: 202 }));
  expect(res.status).toBe(303);
  expect(res.headers.get("location")).toBe(`/orders/TS-7K2QF?t=${TOK}&sent=1`);
  expect(new URL(calls[0].url).pathname).toBe("/v1/orders/TS-7K2QF/email");
  expect(calls[0].method).toBe("POST");
  expect(await calls[0].json()).toEqual({ token: TOK });
  expect(calls[0].headers.get("x-real-ip")).toBe("203.0.113.9");

  const to = async (code: string) =>
    (await ask(() => apiError(409, code))).res.headers.get("location");
  expect(await to("email_on_its_way")).toBe(`/orders/TS-7K2QF?t=${TOK}&sent=pending`);
  expect(await to("too_soon")).toBe(`/orders/TS-7K2QF?t=${TOK}&sent=soon`);
  // Nothing to send: back to the page, which no longer offers it, with no notice.
  expect(await to("no_tickets")).toBe(`/orders/TS-7K2QF?t=${TOK}`);

  const lost = await ask(() => apiError(404, "not_found"));
  expect(lost.res.status).toBe(404);
  expect(await lost.res.text()).toContain("That link doesn't open an order.");
  expect((await ask(() => apiError(429, "rate_limited"))).res.status).toBe(429);
  const { a, calls: none } = app(() => new Response(null, { status: 202 }));
  expect((await a.request("/orders/TS-7K2QF/email", form({ t: "short" }))).status).toBe(404);
  expect(none).toHaveLength(0);

  const notice = async (sent: string) => {
    const { a } = app(() => Response.json(order));
    return (await a.request(`/orders/TS-7K2QF?t=${TOK}&sent=${sent}`)).text();
  };
  expect(await notice("1")).toContain(
    "On its way to ada@example.com. Give it a minute or two, and check spam if it doesn't turn up.",
  );
  expect(await notice("soon")).toContain(
    "We tried sending these less than 10 minutes ago. Wait until it's been 10 minutes, then try again.",
  );
  expect(await notice("pending")).toContain(
    "We're still trying to send the latest one. Give it a little while, then check your spam folder too.",
  );
  expect(await notice("other")).not.toContain('role="status"');
});

// Spec §13.7: a suspended organizer's event is the same plain page for every
// way in, and says nothing about who or why.
const UNAVAILABLE = [
  "<title>Not available",
  "This event isn't available.",
  "You can't buy tickets for it here. Already have an order? Your order link still works.",
];
const unavailable = () => apiError(410, "event_unavailable");

for (const [name, path, api, init] of [
  ["the event page", "/acme/gala", unavailable, undefined],
  ["buying", "/acme/gala/checkout", routes(unavailable), form({ qty_t1: "2", idem: KEY })],
  ["the checkout page", `/checkout/${TOK}`, unavailable, undefined],
] as const) {
  test(`a suspended organizer's event: ${name} is the 410 unavailable page`, async () => {
    const { a } = app(api);
    const res = await a.request(path, init);
    const html = await res.text();
    expect(res.status).toBe(410);
    for (const words of UNAVAILABLE) expect(html).toContain(words);
    expect(html).not.toContain("Acme");
    expect(html).not.toContain("Spring Gala");
    expect(html).not.toContain("API words");
  });
}

test("a suspended organizer's buyers can still ask for their link again", async () => {
  const { a, calls } = app((r) =>
    new URL(r.url).pathname.endsWith("/order-links")
      ? new Response(null, { status: 204 })
      : unavailable(),
  );
  const res = await a.request("/acme/gala/order-link");
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).toContain("Get your ticket link again");
  expect(html).toContain('action="/acme/gala/order-link"');
  const sent = await a.request("/acme/gala/order-link", form({ email: "ada@example.com" }));
  expect(sent.status).toBe(200);
  expect(await sent.text()).toContain(
    "Done. If that address bought tickets for this event, the link is on its way.",
  );
  expect(calls.some((c) => new URL(c.url).pathname.endsWith("/order-links"))).toBe(true);
});

test("back from the bank to a suspended organizer's checkout: the 410 page promises the refund", async () => {
  for (const query of ["payment_intent=pi_1&redirect_status=succeeded", "redirect_status=failed"]) {
    const { a } = app(unavailable);
    const res = await a.request(`/checkout/${TOK}?${query}`);
    const html = await res.text();
    expect(res.status).toBe(410);
    expect(html).toContain("This event isn't available.");
    expect(html).toContain("If your card was charged, we'll refund all of it.");
    expect(html).not.toContain("Already have an order?");
  }
});

test("the plain 410 page has no refund line", async () => {
  for (const path of [`/checkout/${TOK}`, "/acme/gala"]) {
    const { a } = app(unavailable);
    const html = await (await a.request(path)).text();
    expect(html).not.toContain("refund");
  }
});

test("priceNote words what's inside the price", () => {
  expect(priceNote(100, 0)).toBe("Includes $1.00 service fee");
  expect(priceNote(90, 165)).toBe("Includes $0.90 service fee and $1.65 tax");
  expect(priceNote(0, 165)).toBe("Includes $1.65 tax");
  expect(priceNote(0, 0)).toBe("");
});

test("a discount is derived when the API sends all-in prices but no all-in discount", async () => {
  const c = checkout();
  const { all_in_discount_cents, ...quote } = c.quote;
  const html = await (
    await app(() => Response.json({ ...c, quote })).a.request(`/checkout/${TOK}`)
  ).text();
  expect(html).toContain("−$5.10");
  expect(html).not.toMatch(/NaN|undefined/);
});
