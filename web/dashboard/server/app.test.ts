import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createApp } from "./app";

type Call = {
  url: string;
  auth: string | null;
  realIp: string | null;
  xff: string | null;
  ua: string | null;
  cookie: string | null;
  idem: string | null;
};
const stops: (() => void)[] = [];
afterEach(() => {
  for (const s of stops.splice(0)) s();
});

function fakeApi(opts: { loginFails?: boolean } = {}) {
  const calls: Call[] = [];
  const server = Bun.serve({
    port: 0,
    fetch(req) {
      const u = new URL(req.url);
      calls.push({
        url: u.pathname + u.search,
        auth: req.headers.get("authorization"),
        realIp: req.headers.get("x-real-ip"),
        xff: req.headers.get("x-forwarded-for"),
        ua: req.headers.get("user-agent"),
        cookie: req.headers.get("cookie"),
        idem: req.headers.get("idempotency-key"),
      });
      if (u.pathname === "/v1/auth/login" && opts.loginFails)
        return Response.json(
          { error: { code: "invalid_credentials", message: "no" } },
          { status: 401 },
        );
      if (u.pathname === "/v1/auth/login")
        return Response.json({ session_token: "SECRET", user: { id: "u1" } });
      if (u.pathname === "/v1/auth/signup")
        return Response.json({ session_token: "SECRET", user: { id: "u1" } }, { status: 201 });
      if (u.pathname === "/v1/leaky") return Response.json({ session_token: "SECRET", ok: true });
      return Response.json({ ok: true });
    },
  });
  stops.push(() => server.stop());
  return { url: `http://localhost:${server.port}/v1`, calls };
}

const mk = (api: { url: string }, extra: Partial<Parameters<typeof createApp>[0]> = {}) =>
  createApp({ apiUrl: api.url, staticDir: "./dist", secureCookies: false, ...extra });

async function csrf(app: ReturnType<typeof createApp>) {
  const res = await app.request("/");
  return (res.headers.get("set-cookie") ?? "").match(/ts_csrf=([^;]+)/)?.[1] ?? "";
}

const post = (token: string, headers: Record<string, string> = {}, body = "{}") => ({
  method: "POST",
  headers: {
    "content-type": "application/json",
    "x-csrf-token": token,
    cookie: `ts_csrf=${token}`,
    ...headers,
  },
  body,
});

test("strips session token and sets HttpOnly cookie", async () => {
  const api = fakeApi();
  const app = mk(api, { secureCookies: true });
  const t = await csrf(app);
  const res = await app.request("/session/login", post(t, { cookie: `__Host-ts_csrf=${t}` }));
  const body = await res.text();
  expect(res.status).toBe(200);
  expect(body).not.toContain("SECRET");
  expect(body).toContain("u1");
  const set = res.headers.get("set-cookie") ?? "";
  expect(set).toContain("__Host-ts_session=SECRET");
  expect(set).toContain("HttpOnly");
  expect(set).toContain("Secure");
  expect(set).toContain("SameSite=Lax");
  expect(set).toContain("Max-Age=2592000");
});

test("signup keeps 201; API errors pass through unchanged without a cookie", async () => {
  const api = fakeApi();
  const app = mk(api);
  const t = await csrf(app);
  const s = await app.request("/session/signup", post(t));
  expect(s.status).toBe(201);
  expect(s.headers.get("set-cookie")).toContain("ts_session=SECRET");
  const down = mk({ url: "http://127.0.0.1:1/v1" });
  const r = await down.request("/session/login", post(await csrf(down)));
  expect(r.status).toBe(502);
});

test("csrf cookie attributes", async () => {
  const app = mk(fakeApi());
  const set = (await app.request("/")).headers.get("set-cookie") ?? "";
  expect(set).toContain("SameSite=Strict");
  expect(set).not.toContain("HttpOnly");
});

test("rejects writes without a matching CSRF token", async () => {
  const api = fakeApi();
  const app = mk(api);
  const none = await app.request("/api/organizers", { method: "POST", body: "{}" });
  expect(none.status).toBe(403);
  expect((await none.json()).error.code).toBe("csrf");
  const wrong = await app.request("/api/organizers", post("a", { cookie: "ts_csrf=b" }));
  expect(wrong.status).toBe(403);
  expect(api.calls.length).toBe(0);
});

test("proxy adds the bearer from the cookie, drops client auth/cookies, keeps query and UA", async () => {
  const api = fakeApi();
  const app = mk(api);
  await app.request("/api/me?x=1&y=2", {
    headers: {
      cookie: "ts_session=abc",
      authorization: "Bearer evil",
      "user-agent": "tester",
      "x-forwarded-for": "6.6.6.6",
    },
  });
  expect(api.calls[0]).toMatchObject({
    url: "/v1/me?x=1&y=2",
    auth: "Bearer abc",
    ua: "tester",
    xff: null,
  });
  expect(api.calls[0]?.cookie).toBeNull();
});

test("client IP: X-Real-IP trusted only from a loopback peer", async () => {
  const api = fakeApi();
  const hdr = { "x-real-ip": "9.9.9.9" };
  await mk(api, { peerIp: () => "127.0.0.1" }).request("/api/a", { headers: hdr });
  await mk(api, { peerIp: () => "::1" }).request("/api/a", { headers: hdr });
  await mk(api, { peerIp: () => "203.0.113.5" }).request("/api/a", { headers: hdr });
  await mk(api, { peerIp: () => "127.0.0.1" }).request("/api/a");
  expect(api.calls.map((c) => c.realIp)).toEqual([
    "9.9.9.9",
    "9.9.9.9",
    "203.0.113.5",
    "127.0.0.1",
  ]);
});

test("rejects path traversal", async () => {
  const api = fakeApi();
  const app = mk(api);
  // Literal and %2e-encoded dot segments are normalised away by URL parsing, so they miss /api/:
  // a 404, or the SPA's index.html when a built dist/ is present. Either way, never a proxied answer.
  // Encoded separators survive parsing and must be refused by us (400). None may reach the API.
  for (const p of ["/api/%2e%2e/auth/login", "/api/../auth/login"]) {
    const res = await app.request(p);
    expect([200, 404]).toContain(res.status);
    const body = await res.text();
    expect(body).not.toContain("session_token");
    expect(body).not.toContain('"ok":true');
  }
  for (const p of ["/api/%2E%2E%2Fx", "/api/a%2f..%2fb", "/api/a%5C..%5Cb", "/api/%2e%2e%2fauth"]) {
    expect((await app.request(p)).status).toBe(400);
  }
  expect(api.calls.length).toBe(0);
});

test("body over 1 MiB is refused", async () => {
  const api = fakeApi();
  const app = mk(api);
  const res = await app.request("/api/x", post(await csrf(app), {}, "x".repeat(1024 * 1024 + 1)));
  expect(res.status).toBe(413);
  expect(api.calls.length).toBe(0);
});

test("logout forwards the bearer and clears the cookie", async () => {
  const api = fakeApi();
  const app = mk(api);
  const t = await csrf(app);
  const res = await app.request(
    "/session/logout",
    post(t, { cookie: `ts_csrf=${t}; ts_session=abc` }),
  );
  expect(api.calls[0]).toMatchObject({ url: "/v1/auth/logout", auth: "Bearer abc" });
  expect(res.headers.get("set-cookie")).toContain("ts_session=;");
});

test("static: files, SPA fallback, no dotfiles, security headers", async () => {
  const dir = mkdtempSync(join(tmpdir(), "dash-"));
  writeFileSync(join(dir, "index.html"), "<p>spa</p>");
  writeFileSync(join(dir, "app.js"), "1");
  writeFileSync(join(dir, ".env"), "SECRET");
  const app = createApp({ apiUrl: "http://x/v1", staticDir: dir, secureCookies: false });
  expect(await (await app.request("/app.js")).text()).toBe("1");
  expect(await (await app.request("/events/42")).text()).toBe("<p>spa</p>");
  expect((await app.request("/.env")).status).toBe(404);
  expect((await app.request("/%2eenv")).status).toBe(404);
  expect((await app.request("/missing.js")).status).toBe(404);
  const res = await app.request("/");
  expect(res.headers.get("x-frame-options")).toBe("DENY");
  expect(res.headers.get("x-content-type-options")).toBe("nosniff");
  expect(res.headers.get("referrer-policy")).toBe("strict-origin-when-cross-origin");
  expect(res.headers.get("content-security-policy")).toContain("frame-ancestors 'none'");
  const proxied = await mk(fakeApi()).request("/api/me");
  expect(proxied.headers.get("x-frame-options")).toBe("DENY");
});

test("hashed assets are cached for a year; the shell and SPA routes are not", async () => {
  const dir = mkdtempSync(join(tmpdir(), "dash-"));
  writeFileSync(join(dir, "index.html"), "<p>spa</p>");
  mkdirSync(join(dir, "assets"));
  writeFileSync(join(dir, "assets", "x.wasm"), new Uint8Array([0, 0x61, 0x73, 0x6d]));
  const app = createApp({ apiUrl: "http://x/v1", staticDir: dir, secureCookies: false });
  const wasm = await app.request("/assets/x.wasm");
  expect(wasm.status).toBe(200);
  expect(wasm.headers.get("content-type")).toBe("application/wasm");
  expect(wasm.headers.get("cache-control")).toContain("immutable");
  for (const path of ["/", "/index.html", "/events/42"]) {
    expect((await app.request(path)).headers.get("cache-control")).toBeNull();
  }
});

test("login API error passes through unchanged with no cookie", async () => {
  const app = mk(fakeApi({ loginFails: true }));
  const res = await app.request("/session/login", post(await csrf(app)));
  expect(res.status).toBe(401);
  expect(await res.json()).toEqual({ error: { code: "invalid_credentials", message: "no" } });
  expect(res.headers.get("set-cookie")).toBeNull();
});

test("logout with the API down still clears the cookie", async () => {
  const app = mk({ url: "http://127.0.0.1:1/v1" });
  const t = await csrf(app);
  const res = await app.request(
    "/session/logout",
    post(t, { cookie: `ts_csrf=${t}; ts_session=abc` }),
  );
  expect(res.status).toBe(204);
  expect(res.headers.get("set-cookie")).toContain("ts_session=;");
});

test("OPTIONS needs a CSRF token too", async () => {
  const api = fakeApi();
  const res = await mk(api).request("/api/me", { method: "OPTIONS" });
  expect(res.status).toBe(403);
  expect(api.calls.length).toBe(0);
});

test("secure mode reads only the __Host- cookies", async () => {
  const api = fakeApi();
  const app = mk(api, { secureCookies: true });
  const set = (await app.request("/")).headers.get("set-cookie") ?? "";
  expect(set).toContain("__Host-ts_csrf=");
  // An attacker-planted ts_csrf / ts_session from a sibling subdomain is ignored.
  const bad = await app.request("/api/x", post("evil", { cookie: "ts_csrf=evil" }));
  expect(bad.status).toBe(403);
  await app.request("/api/me", {
    headers: { cookie: "ts_session=attacker; __Host-ts_session=real" },
  });
  expect(api.calls[0]?.auth).toBe("Bearer real");
});

test("non-ASCII CSRF mismatch is a 403, not a 500", async () => {
  const app = mk(fakeApi());
  const res = await app.request("/api/x", post("é", { cookie: "ts_csrf=a" }));
  expect(res.status).toBe(403);
});

test("only allowlisted request headers reach the API", async () => {
  const seen: Record<string, string>[] = [];
  const server = Bun.serve({
    port: 0,
    fetch(req) {
      seen.push(Object.fromEntries(req.headers));
      return Response.json({});
    },
  });
  stops.push(() => server.stop());
  const app = mk({ url: `http://localhost:${server.port}/v1` });
  await app.request("/api/me", {
    headers: {
      cookie: "ts_session=abc",
      "x-http-method-override": "DELETE",
      "x-forwarded-host": "evil",
      "x-original-url": "/admin",
      forwarded: "for=1.1.1.1",
      "proxy-authorization": "x",
      "x-custom": "1",
      accept: "application/json",
    },
  });
  const h = seen[0] ?? {};
  for (const k of [
    "x-http-method-override",
    "x-forwarded-host",
    "x-original-url",
    "forwarded",
    "proxy-authorization",
    "x-custom",
    "cookie",
  ])
    expect(h[k]).toBeUndefined();
  expect(h.accept).toBe("application/json");
  expect(h.authorization).toBe("Bearer abc");
});

test("the proxy refuses the API's session-creating routes, however the path is spelled", async () => {
  const api = fakeApi();
  const app = mk(api);
  const t = await csrf(app);
  for (const p of [
    "/api/auth/login",
    "/api/auth/signup",
    "/api/auth/passkeys/login/0197a2b4-0000-7000-8000-000000000000",
    "/api/auth/%6cogin",
    "/api/auth//login",
    "/api/auth/login/",
    "/api/auth/signup?x=1",
  ]) {
    const res = await app.request(p, post(t));
    expect(res.status, p).toBe(404);
    expect(await res.text(), p).not.toContain("SECRET");
  }
  expect(api.calls.length).toBe(0);
  // Starting a passkey sign-in creates no session, so it still goes through.
  expect((await app.request("/api/auth/passkeys/login", post(t))).status).toBe(200);
});

test("the proxy strips a session_token from any JSON response", async () => {
  const app = mk(fakeApi());
  const res = await app.request("/api/leaky");
  expect(res.status).toBe(200);
  expect(await res.json()).toEqual({ ok: true });
});

test("proxy forwards the Idempotency-Key", async () => {
  const api = fakeApi();
  await mk(api).request("/api/organizers", post("a", { "idempotency-key": "k-1" }));
  expect(api.calls[0]?.idem).toBe("k-1");
});

const PLAIN_CSP =
  "default-src 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'; script-src 'self' 'wasm-unsafe-eval'";

test("/config.json tells the page the Turnstile site key", async () => {
  const on = mk(fakeApi(), { turnstileSiteKey: "1x00000000000000000000AA" });
  const res = await on.request("/config.json");
  expect(res.status).toBe(200);
  expect(await res.json()).toEqual({ turnstileSiteKey: "1x00000000000000000000AA" });
  expect(res.headers.get("content-security-policy")).toBe(PLAIN_CSP);
  expect(await (await mk(fakeApi()).request("/config.json")).json()).toEqual({
    turnstileSiteKey: "",
  });
});

test("only the HTML shell lets Turnstile in, and only when it's on", async () => {
  const dir = mkdtempSync(join(tmpdir(), "dash-"));
  writeFileSync(join(dir, "index.html"), "<p>spa</p>");
  const api = fakeApi();
  const on = mk(api, { staticDir: dir, turnstileSiteKey: "1x00000000000000000000AA" });
  for (const path of ["/", "/signin"]) {
    const csp = (await on.request(path)).headers.get("content-security-policy") ?? "";
    expect(csp).toContain(
      "script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; connect-src 'self' https://challenges.cloudflare.com",
    );
    expect(csp).toContain("default-src 'self'");
    expect(csp).toContain("frame-ancestors 'none'");
  }
  expect((await on.request("/api/me")).headers.get("content-security-policy")).toBe(PLAIN_CSP);
  const off = mk(api, { staticDir: dir });
  expect((await off.request("/")).headers.get("content-security-policy")).toBe(PLAIN_CSP);
});

test("the shell allows WebAssembly and nothing more", async () => {
  const dir = mkdtempSync(join(tmpdir(), "dash-"));
  writeFileSync(join(dir, "index.html"), "<p>spa</p>");
  for (const turnstileSiteKey of ["", "1x00000000000000000000AA"]) {
    const csp =
      (await mk(fakeApi(), { staticDir: dir, turnstileSiteKey }).request("/")).headers.get(
        "content-security-policy",
      ) ?? "";
    expect(csp).toContain("'wasm-unsafe-eval'");
    expect(csp).not.toContain("'unsafe-eval'");
    expect(csp).not.toContain("'unsafe-inline'");
    // One script-src only: a second one would be ignored by the browser.
    expect(csp.match(/script-src/g)).toHaveLength(1);
  }
});
