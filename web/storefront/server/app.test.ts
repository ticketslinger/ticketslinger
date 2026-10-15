import { expect, spyOn, test } from "bun:test";
import { createApp } from "./app";

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

test("event page is server-rendered with no script of our own", async () => {
  const { a } = app(() => Response.json(event));
  const res = await a.request("/acme/gala");
  const html = await res.text();
  expect(res.status).toBe(200);
  expect(html).toContain("Spring Gala");
  expect(html).toContain("$25.00");
  expect(html).not.toMatch(/<script[^>]+src="\/assets\//);
  expect(html).not.toMatch(/style="/);
  // The quantity picker can't offer more than are left.
  expect(html).toContain('<option value="3">3</option>');
  expect(html).not.toContain('<option value="4">');
  expect(res.headers.get("content-security-policy")).toContain(
    "script-src 'self' https://challenges.cloudflare.com",
  );
  expect(html).toContain('data-sitekey="1x00000000000000000000AA"');
  expect(html).toContain("A service fee is added at checkout.");
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
      discount_cents: 250,
      tax_cents: 0,
      fee_cents: 95,
      charge_cents: 2250,
      tax_included: false,
    })),
    subtotal_cents: 5000,
    discount_cents: 500,
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
  expect(html).toContain("Service fee");
  expect(html).toContain("$1.90");
  expect(html).toContain("$46.90");
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
  expect(JSON.parse(block?.[1] ?? "").status).toBe("paying");
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
  event: {
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
      qr: "x".repeat(22),
    },
    {
      id: "k2",
      ticket_type_name: "General",
      attendee_name: "<b>Grace</b>",
      status: "valid",
      qr: "y".repeat(22),
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
  expect(html).toContain("Ticket 1: General");
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

const taxed = (inclusive: boolean) => {
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
      tickets: [{ ticket_type_id: "t1", ...t }],
      subtotal_cents: 2500,
      discount_cents: 0,
      tax_cents: t.tax_cents,
      fee_cents: t.fee_cents,
      total_cents: t.charge_cents + t.fee_cents,
      included_tax_cents: t.tax_included ? t.tax_cents : 0,
      fee_mode: "pass",
    },
  });
};

test("tax on top is its own line, and the lines add up", async () => {
  const { a } = app(() => Response.json(taxed(false)));
  const html = await (await a.request(`/checkout/${TOK}`)).text();
  expect(html).toContain("<span>Tax</span><span>$2.06</span>");
  expect(html).not.toContain("Includes");
  expect(sumsUp(html)).toEqual({ sum: 2806, total: 2806 });
});

test("included tax comes from the API's flag", async () => {
  const one = async (t: Record<string, unknown>, included: number) => {
    const c = taxed(true);
    const quote = {
      ...c.quote,
      tickets: [{ ticket_type_id: "t1", discount_cents: 0, fee_cents: 0, ...t }],
      included_tax_cents: included,
    };
    const body = { ...c, quote };
    return (await app(() => Response.json(body)).a.request(`/checkout/${TOK}`)).text();
  };
  // 0% inclusive tax: charge equals the price either way, only the flag says so.
  const zero = await one(
    { price_cents: 1000, tax_cents: 0, charge_cents: 1000, tax_included: true },
    0,
  );
  expect(zero).not.toContain("<span>Tax</span>");
  expect(zero).not.toContain("Includes");
  const inside = await one(
    { price_cents: 2200, tax_cents: 200, charge_cents: 2200, tax_included: true },
    200,
  );
  expect(inside).toContain("Includes $2.00 tax");
  expect(inside).not.toContain("<span>Tax</span>");
  const added = await one(
    { price_cents: 2000, tax_cents: 160, charge_cents: 2160, tax_included: false },
    0,
  );
  expect(added).toContain("<span>Tax</span><span>$1.60</span>");
});

test("tax inside the price is noted under the total, and the lines add up", async () => {
  const { a } = app(() => Response.json(taxed(true)));
  const html = await (await a.request(`/checkout/${TOK}`)).text();
  expect(html).not.toContain("<span>Tax</span>");
  expect(html).toContain("Includes $1.91 tax");
  expect(sumsUp(html)).toEqual({ sum: 2596, total: 2596 });
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
