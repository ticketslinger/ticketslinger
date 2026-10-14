import { expect, test } from "bun:test";
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
    peerIp: () => opts.peer ?? "127.0.0.1",
    fetch: async (r) => {
      calls.push(r.clone());
      return api(r);
    },
  });
  return { a, calls };
}

const KEY = "0f8fad5b-d9cb-469f-a165-70867728950e";
const apiError = (status: number, code: string, extra: Record<string, unknown> = {}) =>
  Response.json({ error: { code, message: "<b>API words</b>", ...extra } }, { status });

// Answers the storefront read with the event, and createCheckout with `checkout`.
const routes =
  (checkout: () => Response, ev: unknown = event): Api =>
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
  const { a, calls } = app(() => Response.json({ token: "tok", status: "open" }, { status: 201 }));
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
  expect(res.headers.get("location")).toBe("/checkout/tok");
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
    const { a, calls } = app(() => Response.json({ token: "tok" }, { status: 201 }));
    const f: Record<string, string> = { qty_t1: "1" };
    if (idem !== undefined) f.idem = idem;
    expect((await a.request("/acme/gala/checkout", form(f))).status).toBe(303);
    expect(calls[0].headers.get("idempotency-key")).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );
  }
});

test("only whole quantities 1-100 are sent, and the code with them", async () => {
  const { a, calls } = app(() => Response.json({ token: "tok" }, { status: 201 }));
  await a.request(
    "/acme/gala/checkout",
    form({
      qty_t1: "2",
      qty_t2: "0",
      qty_t3: "1.5",
      qty_t4: "101",
      qty_t5: "-1",
      qty_t6: "abc",
      other: "9",
      code: "FRIENDS",
      idem: KEY,
    }),
  );
  expect(await calls[0].json()).toEqual({
    organizer: "acme",
    event: "gala",
    items: [{ ticket_type_id: "t1", quantity: 2 }],
    promo_code: "FRIENDS",
  });
});

test("no quantities chosen asks for one, without trying to buy", async () => {
  const { a, calls } = app(routes(() => Response.json({ token: "tok" }, { status: 201 })));
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
  [422, "promo_not_applicable", "That code doesn't apply to these tickets."],
  [422, "turnstile_failed", "One quick check that you're a person."],
  [422, "quantity_out_of_range", "Something went wrong on our side. Try again."],
  [500, "internal", "Something went wrong on our side. Try again."],
];

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
  const { a } = app(() => Response.json(event));
  for (const p of ["/acme/gala", "/nope", "/assets/missing.css"]) {
    const res = await a.request(p);
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
