import { createApp } from "./app";

const app = createApp({
  apiUrl: process.env.TS_API_URL ?? "http://127.0.0.1:3000/v1",
  staticDir: process.env.TS_DASHBOARD_STATIC ?? "./dist",
  secureCookies: process.env.NODE_ENV === "production",
});

Bun.serve({
  port: Number(process.env.TS_DASHBOARD_PORT ?? 3002),
  // Hono passes the second argument through as c.env, which is how app.ts finds the peer address.
  fetch: (req, server) => app.fetch(req, { server }),
});
