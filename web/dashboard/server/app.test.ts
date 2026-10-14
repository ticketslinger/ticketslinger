import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
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
};
const stops: (() => void)[] = [];
afterEach(() => {
  for (const s of stops.splice(0)) s();
});

function fakeApi() {
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
      });
      if (u.pathname === "/v1/auth/login")
        return Response.json({ session_token: "SECRET", user: { id: "u1" } });
      if (u.pathname === "/v1/auth/signup")
        return Response.json({ session_token: "SECRET", user: { id: "u1" } }, { status: 201 });
      if (u.pathname === "/v1/auth/bad")
        return Response.json({ error: { code: "nope", message: "no" } }, { status: 401 });
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
  const res = await app.request("/session/login", post(await csrf(app)));
  const body = await res.text();
  expect(res.status).toBe(200);
  expect(body).not.toContain("SECRET");
  expect(body).toContain("u1");
  const set = res.headers.get("set-cookie") ?? "";
  expect(set).toContain("ts_session=SECRET");
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
  // /session/login only maps to /auth/login, so exercise the error path with a failing upstream.
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
  // Literal and %2e-encoded dot segments are normalised away by URL parsing (so they miss /api/ -> 404);
  // encoded separators survive parsing and must be refused by us (400). None may reach the API.
  for (const p of ["/api/%2e%2e/auth/login", "/api/../auth/login"]) {
    expect((await app.request(p)).status).toBe(404);
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
