import { createApp } from "./app";
import { serverOptions } from "./options";

const opts = serverOptions(process.env);
const app = createApp(opts.app);

Bun.serve({
  hostname: opts.hostname,
  port: opts.port,
  // Hono passes the second argument through as c.env, which is how app.ts finds the peer address.
  fetch: (req, server) => app.fetch(req, { server }),
});
